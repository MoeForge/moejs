package engine

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRequiredLiteral checks which literal a pattern requires, how far
// after a match start it may begin, and that nothing optional, alternative,
// repeated, looked around or referenced back yields one.
func TestRequiredLiteral(t *testing.T) {
	for _, tc := range []struct {
		pattern, flags string
		lit            string // "" for none; folded letters upper case
		pre            int
	}{
		{`(^|[^\\])"\\\/Date\((-?[0-9]+)\)\\\/"`, "g", `"\/Date(`, 1},
		{`\s?;\s?`, "", ";", 1},
		{`TNQP=([^;]*)`, "i", "TNQP=", 0},
		{`TNQP=([^;]*)`, "", "TNQP=", 0},
		{`a|b`, "", "", 0},
		{`(?:abc|abd)x`, "", "x", 3},
		{`(?:abc)?d`, "", "d", 3},
		{`(?:abc)*d`, "", "d", -1},
		{`(abc)+d`, "", "d", -1},
		{`a{2}b`, "", "b", 2},
		{`(?:ab){1}c`, "", "abc", 0},
		{`(?=xyz)d`, "", "d", 0},
		{`(?<=xyz)d`, "", "d", 0},
		{`(?!xyz)\w`, "", "", 0},
		{`(a)\1bc`, "", "bc", -1},
		{`k`, "i", "K", 0},
		{`k`, "iu", "", 0},
		{`s`, "iv", "", 0},
		{`as`, "iu", "A", 0},
		{`ab`, "iu", "AB", 0},
		{`\u{1F600}x`, "u", "\U0001F600x", 0},
		{`[\u{1F600}a]x`, "u", "x", 2},
		{`.x`, "", "x", 1},
		{`^abc`, "", "", 0},
		{`^\d+abc`, "", "", 0},
		{`^abc`, "m", "abc", 0},
		{`\bfoo\b`, "", "foo", 0},
		{`[a]bc`, "", "abc", 0},
		{`[aA]bc`, "", "Abc", 0},
		{`a.c`, "s", "a", 0},
		{`(?:)`, "", "", 0},
	} {
		fl, ok := parseRegExpFlags(tc.flags)
		require.True(t, ok)
		ast, err := parseRegExp(FromGoString(tc.pattern).UTF16(), fl)
		require.NoError(t, err, tc.pattern)
		l := requiredLiteral(ast, fl)
		if tc.lit == "" {
			require.Nil(t, l, "/%s/%s", tc.pattern, tc.flags)
			continue
		}
		require.NotNil(t, l, "/%s/%s", tc.pattern, tc.flags)
		got := make([]uint16, len(l.units))
		for i, c := range l.units {
			if l.fold[i] {
				c -= 0x20
			}
			got[i] = c
		}
		require.Equal(t, FromGoString(tc.lit).UTF16(), got, "/%s/%s", tc.pattern, tc.flags)
		require.Equal(t, tc.pre, l.pre, "/%s/%s pre", tc.pattern, tc.flags)
	}
}

// withoutLiteral returns c without its required literal: the program as it
// runs without the prefilter.
func withoutLiteral(c *compiledRegExp) *compiledRegExp {
	c0 := &compiledRegExp{tr: c.tr, flags: c.flags, key: c.key, exact: c.exact, btCR: c.btCR, btFoldWord: c.btFoldWord, simple: c.simple, run: c.run}
	if !c.exact {
		c0.bt.Store(c.bt.Load())
	}
	c0.fast.Store(&reFast{nat: c.fastPaths().nat})
	return c0
}

// requireSameMatches compares c with and without its literal on s: the
// match from every lastIndex, sticky or not, the global match list and the
// test from 0.
func requireSameMatches(t *testing.T, r *Realm, c *compiledRegExp, s *String, what string) {
	t.Helper()
	require.True(t, sameMatches(t, r, c, s, what))
}

// requireSameMatchesOrInterrupted is requireSameMatches for the fuzzer: a
// comparison the watchdog interrupts (the backtracking VM is exponential
// on some patterns) is skipped.
func requireSameMatchesOrInterrupted(t *testing.T, r *Realm, c *compiledRegExp, s *String, what string) {
	t.Helper()
	sameMatches(t, r, c, s, what)
}

// sameMatches does the comparisons and reports false when the realm was
// interrupted before they finished.
func sameMatches(t *testing.T, r *Realm, c *compiledRegExp, s *String, what string) bool {
	t.Helper()
	var ie *InterruptedError
	check := func(err error) bool {
		if errors.As(err, &ie) {
			return false
		}
		require.NoError(t, err, what)
		return true
	}
	c0 := withoutLiteral(c)
	var sub, sub0 reSubject
	r.initRegExpSubject(&sub, s, c)
	r.initRegExpSubject(&sub0, s, c0)
	for last := 0; last <= s.Len(); last++ {
		for _, sticky := range []bool{false, true} {
			want, err := r.regexpMatchFrom(c0, s, &sub0, last, sticky)
			if !check(err) {
				return false
			}
			got, err := r.regexpMatchFrom(c, s, &sub, last, sticky)
			if !check(err) {
				return false
			}
			require.Equal(t, want, got, "%s from %d sticky=%v", what, last, sticky)
		}
	}
	want, err := c0.findAll(r, &sub0)
	if !check(err) {
		return false
	}
	got, err := c.findAll(r, &sub)
	if !check(err) {
		return false
	}
	for _, m := range want {
		sub0.toUnits(m)
	}
	for _, m := range got {
		sub.toUnits(m)
	}
	require.Equal(t, want, got, "%s global", what)
	if c.flags.sticky {
		return true // matchesFrom0 runs only RegExps neither global nor sticky
	}
	ok0, err := r.matchesFrom0(c0, s)
	if !check(err) {
		return false
	}
	ok, err := r.matchesFrom0(c, s)
	if !check(err) {
		return false
	}
	require.Equal(t, ok0, ok, "%s test", what)
	return true
}

// genLiteralPattern writes a random pattern rich in literal runs, with the
// constructs around them that bound or unbound pre.
func genLiteralPattern(rng *rand.Rand, depth int) string {
	atoms := []string{"a", "b", "A", "k", "s", "x", ";", "=", "\\.", "\\u017F", "\\u212A", "\\uD83D", "\\uDE00", "\\u{1F600}", "é", "\\n", "\\r",
		".", "\\s", "\\w", "\\d", "[ab]", "[^a]", "[kK]", "^", "$", "\\b", "\\B"}
	var b strings.Builder
	n := 1 + rng.IntN(5)
	for range n {
		switch k := rng.IntN(12); {
		case k < 6 || depth > 2:
			b.WriteString(atoms[rng.IntN(len(atoms))])
		case k == 6:
			b.WriteString("(" + genLiteralPattern(rng, depth+1) + ")")
		case k == 7:
			b.WriteString("(?:" + genLiteralPattern(rng, depth+1) + "|" + genLiteralPattern(rng, depth+1) + ")")
		case k == 8:
			b.WriteString("(?=" + genLiteralPattern(rng, depth+1) + ")")
		case k == 9:
			b.WriteString("(?<!" + genLiteralPattern(rng, depth+1) + ")")
		case k == 10:
			b.WriteString("(" + genLiteralPattern(rng, depth+1) + ")\\1")
		default:
			b.WriteString("(?:" + genLiteralPattern(rng, depth+1) + ")")
		}
		if rng.IntN(4) == 0 {
			b.WriteString([]string{"*", "+", "?", "{2}", "{0,2}", "{1,3}?", "??"}[rng.IntN(7)])
		}
	}
	return b.String()
}

// genLiteralSubject writes a random subject over the characters the
// patterns use, with surrogate pairs and lone halves.
func genLiteralSubject(rng *rand.Rand) *String {
	parts := []string{"a", "b", "A", "B", "k", "K", "s", "S", "x", "X", ";", "=", ".", " ", "\n", "\r", "é", "ſ", "K", "ab", "ka", ";x"}
	var u []uint16
	for range rng.IntN(12) {
		switch rng.IntN(10) {
		case 0:
			u = append(u, 0xD83D, 0xDE00)
		case 1:
			u = append(u, 0xD83D)
		case 2:
			u = append(u, 0xDE00)
		default:
			u = append(u, FromGoString(parts[rng.IntN(len(parts))]).UTF16()...)
		}
	}
	return FromUTF16(u)
}

// TestRequiredLiteralWhole checks which patterns are their literal and
// nothing else, the ones that run no matcher.
func TestRequiredLiteralWhole(t *testing.T) {
	for _, tc := range []struct {
		pattern, flags string
		whole          bool
	}{
		{`NQ_VQ`, "g", true},
		{`ab`, "i", true},
		{`ab`, "y", true},
		{`(?:ab)c`, "", true},
		{`(ab)`, "", false},
		{`a.c`, "", false},
		{`ab$`, "", false},
		{`\bab`, "", false},
		{`^ab`, "m", false},
		{`a{2}`, "", false}, // no literal
		{`\u{1F600}x`, "u", true},
		{`😀`, "u", true}, // one character, U+1F600
		{`\uD83Dx`, "u", false},
		{`x\uDE00`, "u", false},
		{`\uD83Dx`, "", true},
	} {
		fl, ok := parseRegExpFlags(tc.flags)
		require.True(t, ok)
		ast, err := parseRegExp(FromGoString(tc.pattern).UTF16(), fl)
		require.NoError(t, err, tc.pattern)
		l := requiredLiteral(ast, fl)
		require.Equal(t, tc.whole, l != nil && l.whole, "/%s/%s", tc.pattern, tc.flags)
	}
}

// TestLiteralIndexUnits compares the scan of UTF-16 subjects with a naive
// search, on units whose bytes are those of the key in the other order, or
// 0, and with every unit of the literal as the key.
func TestLiteralIndexUnits(t *testing.T) {
	alphabet := []uint16{'Q', 'a', 0x5151, 0x5100, 0x0151, 0x0100, 0x5161, 0x6151, 0, 0xD83D, 0xDE00}
	rng := rand.New(rand.NewPCG(5, 6))
	naive := func(u, lit []uint16, from int) int {
		for i := from; i+len(lit) <= len(u); i++ {
			if slices.Equal(u[i:i+len(lit)], lit) {
				return i
			}
		}
		return -1
	}
	for range 20000 {
		lit := make([]uint16, 1+rng.IntN(3))
		for i := range lit {
			lit[i] = alphabet[rng.IntN(len(alphabet))]
		}
		u := make([]uint16, rng.IntN(24))
		for i := range u {
			u[i] = alphabet[rng.IntN(len(alphabet))]
		}
		if rng.IntN(2) == 0 && len(u) >= len(lit) {
			copy(u[rng.IntN(len(u)-len(lit)+1):], lit)
		}
		l := &reLiteral{units: lit, fold: make([]bool, len(lit)), key: rng.IntN(len(lit))}
		sub := &reSubject{s: FromUTF16(u), units: true, wide: u}
		for from := 0; from <= len(u); from++ {
			require.Equal(t, naive(u, lit, from), l.index(sub, from, len(u)), "%x in %x from %d key %d", lit, u, from, l.key)
		}
	}
}

// TestRegExpLiteralDifferential compares every pattern with and without its
// required literal on random subjects, ASCII and UTF-16, under every flag
// set that changes matching.
func TestRegExpLiteralDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	iters := 3000
	if testing.Short() {
		iters = 300
	}
	r := NewRealm()
	for range iters {
		pattern := genLiteralPattern(rng, 0)
		flags := []string{"", "i", "m", "s", "u", "iu", "v", "im", "y", "gu", "is", "imsu"}[rng.IntN(12)]
		fl, _ := parseRegExpFlags(flags)
		c, err := r.compileRegExp(FromGoString(pattern), fl)
		if err != nil || c.fastPaths().lit == nil {
			continue
		}
		for range 6 {
			s := genLiteralSubject(rng)
			requireSameMatches(t, r, c, s, "/"+pattern+"/"+flags+" on "+s.GoString())
		}
	}
}

// TestRegExpLiteralSeeds runs the prefilter on the shapes it changes:
// the late start, the literal at the subject's ends, folded letters next
// to U+212A and U+017F, and surrogate pairs before the literal.
func TestRegExpLiteralSeeds(t *testing.T) {
	r := NewRealm()
	for _, tc := range [][3]string{
		{`(^|[^\\])"\\\/Date\((-?[0-9]+)\)\\\/"`, "g", `{"a":"\/Date(12)\/","b":"x\"\/Date(-3)\/"}`},
		{`(^|[^\\])"\\\/Date\((-?[0-9]+)\)\\\/"`, "g", `{"a":"@-62135568000000@","b":"¤"}`},
		{`\s?;\s?`, "", "a; b ;c;"},
		{`TNQP=([^;]*)`, "i", "x=1; tnqp=2; TNQP=3"},
		{`k=`, "iu", "K= k= K="},
		{`s\d`, "i", "ſ1 S2"},
		{`.x`, "u", "\U0001F600x"},
		{`[\u{1F600}a]{2}x`, "u", "a\U0001F600x\U0001F600\U0001F600x"},
		{`\uDE00x`, "u", "\U0001F600x~x"}, // ~ is a lone U+DE00
		{`\bfoo`, "", "afoo foo"},
		{`^foo`, "m", "a\nfoo\rfoo"},
		{`(?<=a)b`, "", "cbab"},
		{`ab`, "y", "xab"},
		{`ab$`, "", "abab"},
		{`(a)\1b`, "", "aab aaab"},
		{`NQ_VQ`, "g", "xNQ_VQyNQ_VNQ_VQ"},
		{`d1`, "gi", "D1d1xd"},
		{`\u{1F600}`, "gu", "\U0001F600a\U0001F600"},
		{`\uD83D`, "g", "\U0001F600~\U0001F600"},
		{`ab`, "y", "abab"},
		{`"\\\/Qngr\(`, "g", `{"a":"\/Qngr(1)\/"}`},
	} {
		fl, ok := parseRegExpFlags(tc[1])
		require.True(t, ok)
		c, err := r.compileRegExp(FromGoString(tc[0]), fl)
		require.NoError(t, err)
		require.NotNil(t, c.fastPaths().lit, tc[0])
		u := FromGoString(tc[2]).UTF16()
		for i, c := range u {
			if c == '~' {
				u[i] = 0xDE00
			}
		}
		for _, s := range []*String{FromUTF16(u), concat(FromGoString("é"), FromUTF16(u))} {
			requireSameMatches(t, r, c, s, "/"+tc[0]+"/"+tc[1]+" on "+s.GoString())
		}
	}
}

// TestRegExpLiteralSpecSteps checks the steps around a matcher call the
// prefilter skips: lastIndex is read and coerced (valueOf runs) for every
// exec, a failing global or sticky exec writes 0 (a non-writable lastIndex
// throws), a non-global one writes nothing, the legacy statics keep the
// last successful match, and replace, split, match and search return what
// a failed match gives. Every subject lacks the pattern's literal. The
// expectations are Node 24's.
func TestRegExpLiteralSpecSteps(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"valueOf, non-global", []string{`var n = 0; var re = /needle/; re.lastIndex = { valueOf() { n++; return 0; } }; var a = [re.exec("haystack"), re.test("haystack"), "haystack".replace(re, "x"), "haystack".match(re), n]; a.join("|")`}, "|false|haystack||4"},
		{"valueOf, wide subject", []string{`var n = 0; var re = /needle/; re.lastIndex = { valueOf() { n++; return 0; } }; [re.exec("hayéstack"), n].join("|")`}, "|1"},
		{"valueOf that throws", []string{`var re = /needle/; re.lastIndex = { valueOf() { throw new Error("li"); } }; var r; try { re.exec("haystack"); } catch (e) { r = e.message; } r`}, "li"},
		{"global exec resets", []string{`var re = /needle/g; re.lastIndex = 3; var a = re.exec("haystack haystack"); [a, re.lastIndex].join("|")`}, "|0"},
		{"sticky exec resets", []string{`var re = /needle/y; re.lastIndex = 2; var a = re.test("xxneedlx"); [a, re.lastIndex].join("|")`}, "false|0"},
		{"sticky past the literal", []string{`var re = /a?needle/y; re.lastIndex = 1; var a = re.test("xxneedle"); [a, re.lastIndex].join("|")`}, "false|0"},
		{"non-global keeps", []string{`var re = /needle/; re.lastIndex = 3; re.exec("haystack"); re.lastIndex`}, "3"},
		{"non-writable, global", []string{`var re = /needle/g; Object.defineProperty(re, "lastIndex", { value: 0, writable: false }); var r = []; try { re.exec("haystack"); } catch (e) { r.push(e instanceof TypeError); } try { "haystack".replace(re, "x"); } catch (e) { r.push(e instanceof TypeError); } r.join()`}, "true,true"},
		{"non-writable, non-global", []string{`var re = /needle/; Object.defineProperty(re, "lastIndex", { value: 0, writable: false }); [re.exec("haystack"), "haystack".replace(re, "x")].join("|")`}, "|haystack"},
		{"statics kept", []string{`/(n)eedle/.exec("a needle!"); /zzz/.exec("haystack"); "haystack".replace(/zzz/g, "x"); "haystack".split(/;\s?/); "haystack".match(/zzz/g); /zzz/.test("haystack"); "hayé".replace(/zzz/g, "x"); [RegExp.lastMatch, RegExp.$1, RegExp.leftContext, RegExp.rightContext, RegExp.input].join()`}, "needle,n,a ,!,a needle!"},
		{"replace", []string{`["haystack".replace(/(^|[^\\])"\\\/Date\((-?[0-9]+)\)\\\/"/g, "$1x"), "hayéstack".replace(/"\\\/Date\(/g, "x"), "haystack".replace(/needle/, () => { throw 1; })].join("|")`}, "haystack|hayéstack|haystack"},
		{"replace calls no replacer", []string{`var n = 0; "haystack".replace(/needle/g, () => n++); "hayé".replace(/needle/g, () => n++); n`}, "0"},
		{"split", []string{`["a,b".split(/\s?;\s?/), "a,b".split(/;/, 1), "".split(/;/), "é,b".split(/x;/)].map(a => a.length + ":" + a.join("/")).join("|")`}, "1:a,b|1:a,b|1:|1:é,b"},
		{"match and search", []string{`["haystack".match(/needle/g), "haystack".search(/needle/), [..."haystack".matchAll(/needle/g)].length].join("|")`}, "|-1|0"},
		{"search restores lastIndex", []string{`var re = /needle/g; re.lastIndex = 5; "haystack".search(re); re.lastIndex`}, "5"},
		{"found after a miss", []string{`var re = /;\s?x/g; [re.exec("a;b"), re.lastIndex, re.exec("a; x").index, re.lastIndex].join("|")`}, "|0|1|4"},
	})
}

// FuzzRegexpLiteral compares a pattern with and without its required
// literal on one subject, from every lastIndex, sticky or not, globally and
// as a test from 0. A subject byte 0xFF stands for a lone U+DE00 and 0xFE
// for a lone U+D83D, so lone surrogates reach the matcher.
func FuzzRegexpLiteral(f *testing.F) {
	for _, s := range [][3]string{
		{`(^|[^\\])"\\\/Date\((-?[0-9]+)\)\\\/"`, "g", `{"a":"\/Date(12)\/"}`},
		{`\s?;\s?`, "", "a; b ;c;"},
		{`TNQP=([^;]*)`, "i", "x=1; tnqp=2"},
		{`k=`, "iu", "K= k= K="},
		{`[\u{1F600}a]{2}x`, "u", "a\U0001F600x"},
		{`\uDE00x`, "u", "\U0001F600x\xffx"},
		{`(a)\1b`, "", "aab aaab"},
		{`(?<=a)b|c`, "y", "cbab"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, pattern, flags, subject string) {
		if len(pattern) > 48 || len(subject) > 48 {
			return
		}
		fl, ok := parseRegExpFlags(flags)
		if !ok {
			return
		}
		r := NewRealm()
		c, err := r.compileRegExp(FromGoString(pattern), fl)
		if err != nil || c.fastPaths().lit == nil {
			return
		}
		u := fuzzUnits(subject)
		watchdog := time.AfterFunc(time.Second, func() { r.Interrupt("fuzz watchdog") })
		defer watchdog.Stop()
		requireSameMatchesOrInterrupted(t, r, c, FromUTF16(u), "/"+pattern+"/"+flags)
	})
}

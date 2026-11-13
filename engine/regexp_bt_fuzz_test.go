package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FuzzRegexpRE2VsBacktrack checks the backtracking VM against RE2 on the
// patterns RE2 runs exactly (reExact): from every lastIndex, sticky or not,
// both report the same match positions and captures (so the same lastIndex
// for g and y), the global match lists agree, and so does the single-class
// fast path where the pattern has one. The VM is exponential on some exact
// patterns (`(a+)+$`), so an input it does not finish within the watchdog
// is skipped; the watchdog never decides a result.
func FuzzRegexpRE2VsBacktrack(f *testing.F) {
	for _, s := range [][3]string{
		{`(a|ab)(c|bcd)(d*)`, "", "abcd"},
		{`(a+)+$`, "", "aaaab"},
		{`^(?:a|b?){3}`, "", "abab"},
		{`(x)?y`, "g", "xyyxy"},
		{`\b\w+\b`, "g", "foo, bar_1 baz"},
		{`(?:(a)+b)*`, "", "aabab"},
		{`(\d+)?`, "y", "12a3"},
		{`a{2,3}?`, "g", "aaaaaaa"},
		{`(ab)+|b`, "i", "ABabB"},
		{`^\s*(\w+)\s*=\s*(.*?)\s*$`, "m", " key = value \nk2=v2"},
		{`[^a]`, "u", "a\U0001f600\xed\xa0\x80b"},
		{`.`, "", "\U0001f600x"},
		{`^b|a$`, "m", "a\nb\r\nb"},
		{`\bk\B`, "iu", "k ſk Kk"},
		{`[\q{abc|ab}x]+`, "v", "abcabxab"},
		{`[\p{L}--[a-z]]+`, "v", "abCDé"},
		{`\p{Lu}+`, "iu", "abCİ"},
		{`[a-z]+`, "i", "Hello K"},
		{`^image(\[\d*\])?$`, "", "image[12]"},
		{`(?:a|b)*?c`, "s", "ab\nc"},
		{`[\s\S]+?x|$`, "g", "a\nbx"},
		{`(?<y>\d{4})-(?<m>\d{2})`, "", "on 1999-12"},
		// Annex B (B.1.2) without u: extended pattern characters, \c and its
		// fallback, legacy octal and identity escapes, \k without a group
		// name, and class ranges with class escapes.
		{`]{2}|}+|a{|x{,2}`, "", "]]}}a{x{,2}"},
		{`\c|\cA|[\c_][\c]`, "i", "\\c\x01\x1f\\c"},
		{`\0\07\377\400\8[\1\8]`, "", "\x00\x07\xff 08\x018"},
		{`\k[\k]\q\u004\x4g\-`, "i", "kKqu004x4g-"},
		{`[\d-a]+[a-\s-b]*[\s-0-A]`, "", "1-a b-0A"},
		{`(a)\3|\8(b)`, "g", "a\x038b"},
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
		p := FromGoString(pattern)
		ast, err := parseRegExp(p.UTF16(), fl)
		if err != nil || !reExact(ast.Root) {
			return
		}
		r := NewRealm()
		c, err := r.compileRegExp(p, fl)
		if err != nil || !c.exact {
			return // RE2 rejected it (size limits): the VM runs it alone
		}
		bt := &compiledRegExp{flags: c.flags, key: c.key, tr: c.tr}
		s := FromGoString(subject)
		var subRE2, subBT reSubject
		r.initRegExpSubject(&subRE2, s, c)
		r.initRegExpSubject(&subBT, s, bt)
		watchdog := time.AfterFunc(time.Second, func() { r.Interrupt("fuzz watchdog") })
		defer watchdog.Stop()
		var ie *InterruptedError
		for last := 0; last <= s.Len(); last++ {
			for _, sticky := range []bool{false, true} {
				want, err := r.regexpMatchFrom(c, s, &subRE2, last, sticky)
				require.NoError(t, err)
				got, err := r.regexpMatchFrom(bt, s, &subBT, last, sticky)
				if errors.As(err, &ie) {
					return
				}
				require.NoError(t, err)
				require.Equal(t, want, got, "/%s/%s on %q from %d sticky=%v", pattern, flags, subject, last, sticky)
				if c.simple != nil && !sticky {
					start, end, ok := c.simple.find(s, last)
					if !ok {
						require.Nil(t, got, "fast path /%s/%s on %q from %d", pattern, flags, subject, last)
					} else {
						require.NotNil(t, got, "fast path /%s/%s on %q from %d", pattern, flags, subject, last)
						require.Equal(t, []int{start, end}, got[:2], "fast path /%s/%s on %q from %d", pattern, flags, subject, last)
					}
				}
			}
		}
		want, err := c.findAll(r, &subRE2)
		require.NoError(t, err)
		got, err := bt.findAll(r, &subBT)
		if errors.As(err, &ie) {
			return
		}
		require.NoError(t, err)
		for _, m := range want {
			subRE2.toUnits(m)
		}
		for _, m := range got {
			subBT.toUnits(m)
		}
		require.Equal(t, want, got, "global /%s/%s on %q", pattern, flags, subject)
	})
}

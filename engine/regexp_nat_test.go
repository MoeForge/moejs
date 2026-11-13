package engine

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// natWork matches c on s from 0 and returns the match and the native
// matcher's work; the subject must run on the native matcher.
func natWork(t *testing.T, r *Realm, c *compiledRegExp, s *String) ([]int, int64) {
	t.Helper()
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat, "the native matcher runs the subject")
	m := &r.regexpState().nat
	m.work = 0
	got, err := r.regexpMatchFrom(c, s, &sub, 0, false)
	require.NoError(t, err)
	return got, m.work
}

// TestNatLinear checks the bound on the patterns plain backtracking takes
// exponential time on: the work at 4n is about four times the work at n,
// for 3000 and 12000 characters, with and without u, on ASCII and UTF-16
// subjects. It counts instructions and characters, never time. Their
// loops push entries, so 10^5 and 4·10^5 characters pass the stack bound
// and run on Go's regexp, linear by construction, with the same results.
func TestNatLinear(t *testing.T) {
	r := NewRealm()
	for _, tc := range []struct {
		pattern, tail string
		want          func(n int) []int
	}{
		{`(a+)+$`, "b", func(n int) []int { return nil }},
		{`(a|aa)*c`, "bc", func(n int) []int { return []int{n + 1, n + 2, -1, -1} }},
		{`(?:a*)*b`, "c", func(n int) []int { return nil }},
		{`^(\w+\s?)*$`, "!", func(n int) []int { return nil }},
	} {
		for _, flags := range []string{"", "u"} {
			fl, _ := parseRegExpFlags(flags)
			c, err := r.compileRegExp(FromGoString(tc.pattern), fl)
			require.NoError(t, err)
			if c.fastPaths().nat == nil {
				// (?:a*)* is not in the RE2 class: the backtracking VM runs it.
				require.False(t, reExact(mustParse(t, tc.pattern, fl).Root), tc.pattern)
				continue
			}
			for _, wide := range []bool{false, true} {
				for _, n := range []int{100000, 400000} {
					text := strings.Repeat("a", n) + tc.tail
					if wide {
						text += "é"
					}
					s := FromGoString(text)
					var sub reSubject
					r.initRegExpSubject(&sub, s, c)
					require.False(t, sub.nat, "/%s/%s n=%d runs on Go's regexp", tc.pattern, flags, n)
					got, err := r.regexpMatchFrom(c, s, &sub, 0, false)
					require.NoError(t, err)
					require.Equal(t, tc.want(n), got, "/%s/%s wide=%v n=%d", tc.pattern, flags, wide, n)
				}
				var works [2]int64
				for i, n := range []int{3000, 12000} {
					text := strings.Repeat("a", n) + tc.tail
					if wide {
						text += "é" // UTF-16, and the run of a's from 0 stays
					}
					s := FromGoString(text)
					got, work := natWork(t, r, c, s)
					want := tc.want(n)
					require.Equal(t, want, got, "/%s/%s wide=%v n=%d", tc.pattern, flags, wide, n)
					works[i] = work
				}
				ratio := float64(works[1]) / float64(works[0])
				t.Logf("/%s/%s wide=%v: work %d at n, %d at 4n (%.2fx)", tc.pattern, flags, wide, works[0], works[1], ratio)
				require.Less(t, ratio, 4.5, "/%s/%s wide=%v", tc.pattern, flags, wide)
				require.Less(t, works[1], int64(64*12000), "/%s/%s wide=%v", tc.pattern, flags, wide)
			}
		}
	}
}

func mustParse(t *testing.T, pattern string, fl regexpFlags) *reAST {
	t.Helper()
	ast, err := parseRegExp(FromGoString(pattern).UTF16(), fl)
	require.NoError(t, err)
	return ast
}

// TestNatInterrupt checks that a long native match polls the interrupt.
func TestNatInterrupt(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`(a+)+$`), regexpFlags{})
	require.NoError(t, err)
	s := FromGoString(strings.Repeat("a", 12000) + "b")
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat)
	r.Interrupt("stop")
	_, err = c.matchAt(r, &sub, 0, false)
	var ie *InterruptedError
	require.True(t, errors.As(err, &ie), "got %v", err)
	for _, w := range r.regexpState().nat.visited {
		require.Zero(t, w, "the visited set is cleared after an interrupted exec")
	}
}

// TestNatCap checks the fallback past the visited-set cap: a subject whose
// set would pass natMaxBits runs on Go's regexp, with the same result.
func TestNatCap(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`(?:[a-h]|x[a-h]){0,12}y(\d)`), regexpFlags{global: true})
	require.NoError(t, err)
	require.NotNil(t, c.fastPaths().nat)
	limit := natMaxBits / c.fastPaths().nat.nstates
	for _, n := range []int{limit - 10, limit + 10} {
		body := strings.Repeat("abcdefgh", n/8+1)[:n-3] + "y7z"
		for _, s := range []*String{FromGoString(body), FromGoString("é" + body[1:])} {
			var sub reSubject
			r.initRegExpSubject(&sub, s, c)
			require.Equal(t, s.Len() < limit, sub.nat, "n=%d", s.Len())
			got, err := r.regexpMatchFrom(c, s, &sub, 0, false)
			require.NoError(t, err)
			require.Equal(t, []int{s.Len() - 15, s.Len() - 1, s.Len() - 2, s.Len() - 1}, got, "n=%d", s.Len())
		}
	}
}

// TestNatMemoryCharge checks that the visited set is charged.
func TestNatMemoryCharge(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(1 << 40)
	c, err := r.compileRegExp(FromGoString(`(a+)+$`), regexpFlags{})
	require.NoError(t, err)
	s := FromGoString(strings.Repeat("a", 12000) + "b")
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat)
	before := r.Stats().AllocatedBytes
	_, err = c.matchAt(r, &sub, 0, false)
	require.NoError(t, err)
	require.GreaterOrEqual(t, r.Stats().AllocatedBytes-before, int64(12000*c.fastPaths().nat.nstates/8))
}

// natOracles returns c on today's routing (RE2 and its exceptions) and on
// the backtracking VM alone, which implements the spec's semantics
// directly.
func natOracles(c *compiledRegExp) (re2, bt *compiledRegExp) {
	re2 = &compiledRegExp{tr: c.tr, flags: c.flags, key: c.key, exact: c.exact, btCR: c.btCR, btFoldWord: c.btFoldWord, simple: c.simple, run: c.run}
	re2.fast.Store(&reFast{})
	bt = &compiledRegExp{tr: c.tr, flags: c.flags, key: c.key}
	bt.fast.Store(&reFast{})
	return re2, bt
}

// sameAsOracles compares c (native) with both oracles on s: the match from
// every lastIndex, sticky or not, the global match list and the test from
// 0. It reports false when the watchdog interrupted the backtracking VM.
func sameAsOracles(t *testing.T, r *Realm, c *compiledRegExp, s *String, what string) bool {
	t.Helper()
	re2, bt := natOracles(c)
	var ie *InterruptedError
	cs := []*compiledRegExp{c, re2, bt}
	subs := make([]reSubject, len(cs))
	for i, x := range cs {
		r.initRegExpSubject(&subs[i], s, x)
	}
	require.True(t, subs[0].nat, what)
	run := func(f func(i int) (any, error)) bool {
		var want any
		for i := range cs {
			got, err := f(i)
			if errors.As(err, &ie) {
				return false
			}
			require.NoError(t, err, what)
			if i == 0 {
				want = got
				continue
			}
			require.Equal(t, got, want, "%s: native against %s", what, []string{"", "today's routing", "backtracking VM"}[i])
		}
		return true
	}
	for last := 0; last <= s.Len(); last++ {
		for _, sticky := range []bool{false, true} {
			if !run(func(i int) (any, error) { return r.regexpMatchFrom(cs[i], s, &subs[i], last, sticky) }) {
				return false
			}
		}
	}
	if !run(func(i int) (any, error) {
		all, err := cs[i].findAll(r, &subs[i])
		for _, m := range all {
			subs[i].toUnits(m)
		}
		return all, err
	}) {
		return false
	}
	if c.flags.sticky {
		return true // matchesFrom0 runs only RegExps neither global nor sticky
	}
	return run(func(i int) (any, error) { return r.matchesFrom0(cs[i], s) })
}

// genNatPattern writes a random pattern of the constructs the native
// matcher compiles, and some it does not.
func genNatPattern(rng *rand.Rand, depth int) string {
	atoms := []string{"a", "b", "A", "k", "x", ";", "\\.", "\\u017F", "\\u212A", "\\uD83D", "\\uDE00", "\\u{1F600}", "é", "\\n", "\\r",
		".", "\\s", "\\S", "\\w", "\\W", "\\d", "[ab]", "[^a]", "[a-z]", "[^\\n]", "[\\u{1F600}a]", "^", "$", "\\b", "\\B"}
	var b strings.Builder
	n := 1 + rng.IntN(5)
	for range n {
		switch k := rng.IntN(14); {
		case k < 7 || depth > 2:
			b.WriteString(atoms[rng.IntN(len(atoms))])
		case k < 9:
			b.WriteString("(" + genNatPattern(rng, depth+1) + ")")
		case k < 11:
			b.WriteString("(?:" + genNatPattern(rng, depth+1) + "|" + genNatPattern(rng, depth+1) + ")")
		case k == 11:
			b.WriteString("(" + genNatPattern(rng, depth+1) + "|" + genNatPattern(rng, depth+1) + ")")
		case k == 12:
			b.WriteString("(?=" + genNatPattern(rng, depth+1) + ")")
		default:
			b.WriteString("(?:" + genNatPattern(rng, depth+1) + ")")
		}
		if rng.IntN(3) == 0 {
			b.WriteString([]string{"*", "+", "?", "{2}", "{0,2}", "{1,3}", "{2,}", "*?", "+?", "??", "{1,3}?", "{2,}?"}[rng.IntN(12)])
		}
	}
	return b.String()
}

// genNatSubject writes a random subject over the patterns' characters.
func genNatSubject(rng *rand.Rand) *String {
	parts := []string{"a", "b", "A", "B", "k", "K", "x", ";", ".", " ", "\n", "\r", "é", "ſ", "K", "ab", "aab", "_", "1", " "}
	var u []uint16
	for range rng.IntN(10) {
		switch rng.IntN(12) {
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

// TestNatDifferential compares the native matcher with today's routing and
// with the backtracking VM on generated patterns and subjects, under every
// flag set that changes matching.
func TestNatDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	iters := 4000
	if testing.Short() {
		iters = 400
	}
	r := NewRealm()
	ran := 0
	for range iters {
		pattern := genNatPattern(rng, 0)
		flags := []string{"", "i", "m", "s", "u", "iu", "v", "im", "y", "gu", "is", "imsu", "mu", "iv"}[rng.IntN(14)]
		fl, _ := parseRegExpFlags(flags)
		c, err := r.compileRegExp(FromGoString(pattern), fl)
		if err != nil || c.fastPaths().nat == nil {
			continue
		}
		ran++
		for range 5 {
			s := genNatSubject(rng)
			watchdog := time.AfterFunc(2*time.Second, func() { r.Interrupt("watchdog") })
			sameAsOracles(t, r, c, s, "/"+pattern+"/"+flags+" on "+s.GoString())
			watchdog.Stop()
			r.ClearInterrupt()
		}
	}
	require.Greater(t, ran, iters/4)
}

// TestNatNotNative lists patterns the native matcher must not run: a
// capture an iteration may skip (JavaScript resets it, RE2 keeps the last
// value), a loop whose body can match empty past min, lookarounds and
// backreferences.
func TestNatNotNative(t *testing.T) {
	r := NewRealm()
	for _, p := range []string{`(?:(a)|b)+`, `(?:(a)?b)*`, `(?:a*)*b`, `(a?)+`, `(?:a|)+`, `(a)\1`, `(?=a)a`, `(?<!a)b`, `a{1001}`} {
		c, err := r.compileRegExp(FromGoString(p), regexpFlags{})
		require.NoError(t, err)
		require.Nil(t, c.fastPaths().nat, p)
	}
}

// TestNatSeeds runs the native matcher on the shapes it treats apart: runs
// that stop at a visited position, lazy runs, captures in loops, counted
// repeats, assertions next to line terminators and surrogates, and u-mode
// characters past the BMP.
func TestNatSeeds(t *testing.T) {
	r := NewRealm()
	for _, tc := range [][3]string{
		{`(((\w+):\/\/)([^\/:]*)(:(\d+))?)?([^#?]*)(\?([^#]*))?(#(.*))?`, "", "http://www.example.com:80/path?q=1#frag"},
		{`^\s*|\s*$`, "g", "  a b  "},
		{`^\s*(\S*(\s+\S+)*)\s*$`, "", "  one two  three "},
		{`\s?;\s?`, "g", "a; b ;c;d"},
		{`(\d*)(\D*)`, "g", "528.9x"},
		{`(a|ab)(c|bcd)(d*)`, "", "abcd"},
		{`(x)?y`, "g", "xyyxy"},
		{`a{2,3}?`, "g", "aaaaaaa"},
		{`(?:ab){2,3}?c`, "", "abababc"},
		{`\w*?x`, "", "aaax"},
		{`[^a]*?b+?`, "g", "ccbbcb"},
		{`(\s+\S+)*\s*$`, "", "a b c !"},
		{`^b|a$`, "m", "a\nb\r\nb a"},
		{`\bk\B`, "iu", "k ſk Kk"},
		{`.\B.`, "u", "\U0001F600\U0001F600a"},
		{`[\u{1F600}a]{2}`, "gu", "a\U0001F600\U0001F600"},
		{`\S+`, "u", "\U0001F600x"},
		{`..`, "g", "\U0001F600x"},
		{`\uD83D`, "g", "\U0001F600\xfex"},
		{`(?:a|b?){3}`, "", "abab"},
		{`(?:x|xy){2}z`, "", "xyxyz"},
		{`[\q{abc|ab}x]+`, "v", "abcabxab"},
		{`(?<y>\d{4})-(?<m>\d{2})`, "d", "on 1999-12"},
	} {
		fl, ok := parseRegExpFlags(tc[1])
		require.True(t, ok)
		c, err := r.compileRegExp(FromGoString(tc[0]), fl)
		require.NoError(t, err)
		require.NotNil(t, c.fastPaths().nat, tc[0])
		u := fuzzUnits(tc[2])
		for _, s := range []*String{FromUTF16(u), FromUTF16(append([]uint16{0xE9}, u...))} {
			require.True(t, sameAsOracles(t, r, c, s, "/"+tc[0]+"/"+tc[1]+" on "+s.GoString()))
		}
	}
}

// FuzzRegexpNative compares the native matcher with today's routing and
// with the backtracking VM on fuzzed patterns and subjects (0xFF stands for
// a lone U+DE00, 0xFE for a lone U+D83D).
func FuzzRegexpNative(f *testing.F) {
	for _, s := range [][3]string{
		{`(((\w+):\/\/)([^\/:]*)(:(\d+))?)?([^#?]*)`, "", "http://a.b:80/p"},
		{`^\s*|\s*$`, "g", "  a  "},
		{`(?:(a)|b)+`, "", "aab"},
		{`\w*?x|\b`, "y", "aaax"},
		{`^b|a$`, "m", "a\nb\r\nb"},
		{`\bk\B`, "iu", "k \xc5\xbfk"},
		{`.\B.`, "u", "\xf0\x9f\x98\x80\xfex"},
		{`(a|ab)(c|bcd)(d*)`, "i", "ABCD"},
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
		if err != nil || c.fastPaths().nat == nil {
			return
		}
		u := fuzzUnits(subject)
		watchdog := time.AfterFunc(time.Second, func() { r.Interrupt("fuzz watchdog") })
		defer watchdog.Stop()
		sameAsOracles(t, r, c, FromUTF16(u), "/"+pattern+"/"+flags)
	})
}

// fuzzUnits returns the UTF-16 code units of a subject given as UTF-8,
// where the invalid bytes 0xFF and 0xFE stand for a lone U+DE00 and a lone
// U+D83D.
func fuzzUnits(subject string) []uint16 {
	var u []uint16
	for i := 0; i < len(subject); {
		r, w := utf8.DecodeRuneInString(subject[i:])
		switch {
		case r == utf8.RuneError && w == 1 && subject[i] == 0xFF:
			u = append(u, 0xDE00)
		case r == utf8.RuneError && w == 1 && subject[i] == 0xFE:
			u = append(u, 0xD83D)
		default:
			u = utf16.AppendRune(u, r)
		}
		i += w
	}
	return u
}

// TestRegExpFastPathsLazy checks that compiling a pattern builds neither
// its literal nor its native program: a script that compiles many patterns
// and runs few pays for the ones it runs.
func TestRegExpFastPathsLazy(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`\s?;x(\d+)`), regexpFlags{})
	require.NoError(t, err)
	require.Nil(t, c.fast.Load())
	ok, err := r.matchesFrom0(c, FromGoString("a ;x12"))
	require.NoError(t, err)
	require.True(t, ok)
	fp := c.fast.Load()
	require.NotNil(t, fp)
	require.NotNil(t, fp.lit)
	require.NotNil(t, fp.nat)
	require.Same(t, fp, c.fastPaths())
}

// TestNatVisitedFollowsSearch checks that a search's visited set covers
// the positions it reaches, not the rest of the subject: a global search and
// a split, which search from each match or position of a 600000-unit
// subject, would otherwise allocate a set the size of the rest each time.
func TestNatVisitedFollowsSearch(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(1 << 40)
	c, err := r.compileRegExp(FromGoString(`\w+`), regexpFlags{global: true})
	require.NoError(t, err)
	s := FromGoString(strings.Repeat("ab ", 200000))
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat)
	require.Greater(t, s.Len()*c.fastPaths().nat.nstates, natKeepBits, "a set for the whole subject is not kept")
	before := r.Stats().AllocatedBytes
	all, err := c.findAll(r, &sub)
	require.NoError(t, err)
	require.Len(t, all, 200000)
	require.Equal(t, []int{599997, 599999}, all[len(all)-1])
	require.Less(t, r.Stats().AllocatedBytes-before, int64(1<<20))
	// split searches from each position with a sticky search.
	before = r.Stats().AllocatedBytes
	v, err := runScriptValue(t, r, `"ab ".repeat(200000).split(/ (?:x)?/).length`)
	require.NoError(t, err)
	require.Equal(t, "200001", v.String())
	require.Less(t, r.Stats().AllocatedBytes-before, int64(64<<20))
}

// TestNatStackBound checks the bound on the backtrack stack: a program
// whose loops push L entries an iteration runs a subject natively up to the
// length at which m + (n + 1) × L entries reach natMaxStack, and one past
// it on Go's regexp; a native search at the bound charges little more than
// the stack's cap; a program without a loop stays native on long subjects.
func TestNatStackBound(t *testing.T) {
	r := NewRealm()
	for _, tc := range []struct {
		pattern string
		pushes  int
	}{
		{`(?:ab|c)*d`, 2},           // the loop's split and the alternation's
		{`^(\w+\s?)*$`, 5},          // split, two saves, two runs
		{`(a+)+$`, 4},               // split, two saves, a run
		{`(?:a(?:bc)*)*d`, 2},       // the outer and the inner split
		{`(\d+)-(\w*)x?\s{0,3}`, 0}, // runs and saves, no loop
	} {
		c, err := r.compileRegExp(FromGoString(tc.pattern), regexpFlags{})
		require.NoError(t, err)
		p := c.fastPaths().nat
		require.NotNil(t, p, tc.pattern)
		require.Equal(t, tc.pushes, p.loopPush, tc.pattern)
		most := natMaxBits/p.nstates - 1
		if tc.pushes > 0 {
			most = min(most, (natMaxStack/natEntrySize-len(p.insts))/tc.pushes-1)
			require.Less(t, most, 100000, tc.pattern)
		} else {
			require.Greater(t, most, 400000, "/%s/ has no loop: only the visited set bounds it", tc.pattern)
		}
		require.True(t, p.fits(most), tc.pattern)
		require.False(t, p.fits(most+1), tc.pattern)
	}

	// At the bound, the deepest stack the pattern pushes, natively.
	c, err := r.compileRegExp(FromGoString(`^(\w+\s?)*$`), regexpFlags{})
	require.NoError(t, err)
	n := c.fastPaths().nat.maxLen / 5 * 5
	s := FromGoString(strings.Repeat("word ", n/5))
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat)
	before := r.Stats().AllocatedBytes
	got, err := r.regexpMatchFrom(c, s, &sub, 0, false)
	require.NoError(t, err)
	require.Equal(t, []int{0, n, n - 5, n}, got)
	require.Less(t, r.Stats().AllocatedBytes-before, int64(2*natMaxStack+natMaxBits/8))
	require.LessOrEqual(t, cap(r.regexpState().nat.stack), natKeepStack, "a large stack is not kept")

	// A program without a loop stays native on a long subject.
	c, err = r.compileRegExp(FromGoString(`(\d+)-(\w*)x?`), regexpFlags{})
	require.NoError(t, err)
	s = FromGoString(strings.Repeat("ab ", 100000) + "12-ab" + strings.Repeat("c", 100000))
	r.initRegExpSubject(&sub, s, c)
	require.True(t, sub.nat)
	got, err = r.regexpMatchFrom(c, s, &sub, 0, false)
	require.NoError(t, err)
	require.Equal(t, []int{300000, s.Len(), 300000, 300002, 300003, s.Len()}, got)
}

// TestNatStackUnderMemoryLimit runs patterns whose loops push entries every
// iteration over subjects long enough that their stack would pass a small
// memory limit: they run on Go's regexp, as before the native matcher, and
// match within the limit.
func TestNatStackUnderMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		limit         int64
		pattern, unit string
		repeat        int
	}{
		{32 << 20, `^(\w+\s?)*$`, "word ", 400000},
		{16 << 20, `^(\w+\s?)*$`, "word ", 400000},
		{16 << 20, `^(?:[^"\\]|\\.)*$`, "a", 500000},
	} {
		r := NewRealm()
		r.SetMemoryLimit(tc.limit)
		c, err := r.compileRegExp(FromGoString(tc.pattern), regexpFlags{})
		require.NoError(t, err)
		require.False(t, c.fastPaths().nat.fits(len(tc.unit)*tc.repeat), "/%s/ on %d units runs on Go's regexp", tc.pattern, len(tc.unit)*tc.repeat)
		src := "/" + tc.pattern + "/.test(" + strconv.Quote(tc.unit) + ".repeat(" + strconv.Itoa(tc.repeat) + "))"
		v, err := runScriptValue(t, r, src)
		require.NoError(t, err, "%s under %d MiB", src, tc.limit>>20)
		require.Equal(t, "true", v.String(), src)
		require.Zero(t, r.regexpState().nat.work, "%s runs no native search", src)
	}
}

// TestNatFirstSearchAllocates checks that a realm's first search of a
// pattern without captures on a few characters takes the matcher's own
// buffers, and that a set which left them for a larger one, and was not
// kept, leaves no bits behind for the next search.
func TestNatFirstSearchAllocates(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`[ab]+[cd]`), regexpFlags{})
	require.NoError(t, err)
	got, _ := natWork(t, r, c, FromGoString("xaac"))
	require.Equal(t, []int{1, 4}, got)
	m := &r.regexpState().nat
	require.Same(t, &m.visited0[0], unsafe.SliceData(m.visited), "the visited set")
	require.Same(t, &m.caps0[0], unsafe.SliceData(m.caps), "the slots")

	long := FromGoString(strings.Repeat("a", 3*natKeepBits/c.fastPaths().nat.nstates) + "e")
	got, _ = natWork(t, r, c, long)
	require.Nil(t, got)
	require.Nil(t, m.visited, "a large set is not kept")
	for range 2 {
		got, _ = natWork(t, r, c, FromGoString("aac"))
		require.Equal(t, []int{0, 3}, got, "a match past a search that left the set")
	}
	r.ReleaseCallData()
	got, _ = natWork(t, r, c, FromGoString("aac"))
	require.Equal(t, []int{0, 3}, got, "a match past a release")
}

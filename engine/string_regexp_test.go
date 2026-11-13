package engine

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runReference is c without its run pattern: the engines it would
// otherwise run on (RE2, or the backtracking VM).
func runReference(c *compiledRegExp) *compiledRegExp {
	ref := &compiledRegExp{flags: c.flags, key: c.key, tr: c.tr, exact: c.exact, btCR: c.btCR, btFoldWord: c.btFoldWord}
	if p := c.bt.Load(); p != nil {
		ref.bt.Store(p)
	}
	return ref
}

// checkRunPattern compares the run pattern of pattern/flags, when it has
// one, with the other engines on subject: the match from every lastIndex,
// sticky and not, the global match list, and test. It reports whether the
// pattern has a run pattern.
func checkRunPattern(t *testing.T, r *Realm, pattern, flags string, subject *String) bool {
	t.Helper()
	fl, ok := parseRegExpFlags(flags)
	require.True(t, ok, flags)
	c, err := r.compileRegExp(FromGoString(pattern), fl)
	if err != nil {
		return false
	}
	ref := runReference(c)
	var sub, refSub reSubject
	r.initRegExpSubject(&sub, subject, c)
	r.initRegExpSubject(&refSub, subject, ref)
	// A hang guard, not a timing assertion: the backtracking VM is
	// exponential on some patterns, and an interrupted reference skips the
	// case (the watchdog never decides a result).
	watchdog := time.AfterFunc(time.Second, func() { r.Interrupt("watchdog") })
	defer watchdog.Stop()
	var ie *InterruptedError
	for last := 0; last <= subject.Len(); last++ {
		for _, sticky := range []bool{false, true} {
			want, err := r.regexpMatchFrom(ref, subject, &refSub, last, sticky)
			if errors.As(err, &ie) {
				r.ClearInterrupt()
				return c.run != nil
			}
			require.NoError(t, err)
			got, err := r.regexpMatchFrom(c, subject, &sub, last, sticky)
			require.NoError(t, err)
			require.Equal(t, want, got, "/%s/%s on %q from %d sticky=%v", pattern, flags, subject.GoString(), last, sticky)
		}
	}
	want, err := ref.findAll(r, &refSub)
	if errors.As(err, &ie) {
		r.ClearInterrupt()
		return c.run != nil
	}
	require.NoError(t, err)
	got, err := c.findAll(r, &sub)
	require.NoError(t, err)
	require.Equal(t, want, got, "global /%s/%s on %q", pattern, flags, subject.GoString())
	if c.run != nil && c.run.begin && sub.ascii {
		ok, err := c.run.test(r, sub.text)
		require.NoError(t, err)
		m, err := ref.matchAt(r, &refSub, 0, false)
		require.NoError(t, err)
		require.Equal(t, m != nil, ok, "test /%s/%s on %q", pattern, flags, subject.GoString())
	}
	return c.run != nil
}

var runSubjects = []string{
	"", "a", "b", "=", "ab", "aab", "abb", "a=", "ab==", "ab===", "AbZ", "a1+/", "aaaa", "a\nb", "a b",
	"data:image/png;base64,iVBORw0KGgo=", "data:image/svg+xml;base64,PHN2Zz4=", "data:image/png;base64,iVBOR w0=", "data:text/plain;base64,QQ==",
}

// runAtoms and runQuants make the patterns of TestRunPatternDifferential:
// characters and classes that overlap in every way, and every quantifier.
var (
	runAtoms  = []string{"a", "b", "=", "[a-z]", "[A-Za-z0-9+/]", "[^a]", `\d`, `\w`, ".", `\/`, "(a)", "(?<n>[ab])", "(?:ab)"}
	runQuants = []string{"", "?", "*", "+", "{2}", "{1,3}", "{0,2}", "{2,}", "*?", "+?"}
)

// TestRunPatternDifferential compares the run patterns of every pattern
// of one or two quantified atoms, anchored or not, under several flags,
// with the other engines, on ASCII and UTF-16 subjects and ropes; and the
// data URL shapes plugins validate.
func TestRunPatternDifferential(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	r := NewRealm()
	subjects := make([]*String, 0, 2*len(runSubjects)+4)
	for _, s := range runSubjects {
		subjects = append(subjects, FromGoString(s))
	}
	subjects = append(subjects, FromGoString("aé="), FromGoString("ab\U0001f600"), FromGoString("Kſ"))
	rope, err := r.Concat(FromGoString(strings.Repeat("ab", 40)), FromGoString("=="))
	require.NoError(t, err)
	subjects = append(subjects, rope)
	var items, pairItems []string
	for _, a := range runAtoms {
		for _, q := range runQuants {
			items = append(items, a+q)
		}
	}
	for _, a := range []string{"a", "=", "[a-z]", "[A-Za-z0-9+/]", "[^a]", ".", "(a)"} {
		for _, q := range []string{"", "?", "*", "+", "{1,3}", "+?"} {
			pairItems = append(pairItems, a+q)
		}
	}
	runs := 0
	check := func(pattern, flags string) {
		for _, s := range subjects {
			if checkRunPattern(t, r, pattern, flags, s) {
				runs++
			}
		}
	}
	for _, x := range pairItems {
		for _, y := range pairItems {
			check("^"+x+y+"$", "")
			check("^"+x+y, "i")
			check(x+y, "y")
		}
	}
	for _, x := range items {
		check("^("+x+")$", "")
		check("^"+x+"$", "s")
		check("^"+x+"$", "u")
		check("^"+x+"$", "m")
		check("^"+x+"$", "dg")
	}
	for _, p := range []string{
		`^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$`,
		`^data:(image\/[a-z0-9.+-]+);base64,([A-Za-z0-9+/]+={0,2})$`,
		`^data:(?<mime>[\w.+-]+\/[\w.+-]+);base64,(?<data>[A-Za-z0-9+/=]+)$`,
		`^[A-Za-z0-9+/]*={0,2}$`,
		`^https?:\/\/`,
		`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`,
		`^[A-Za-z0-9+/]{1,4096}={0,2}$`,
		`^data:image\/png;base64,`,
		`^\s*$`, `^\S+$`, `^[\d]{4}-\d{2}-\d{2}$`,
	} {
		for _, fl := range []string{"", "i", "g", "y", "d", "u", "v"} {
			check(p, fl)
		}
	}
	require.Greater(t, runs, 20000)
}

// FuzzRunPattern compares the run pattern of a pattern, when it has one,
// with the other engines on a subject (see checkRunPattern).
func FuzzRunPattern(f *testing.F) {
	for _, s := range [][3]string{
		{`^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$`, "", "data:image/png;base64,iVBORw0KGgo="},
		{`^(a+)(b?)=*$`, "i", "AAb=="},
		{`[a-z]+=`, "y", "abc=d"},
		{`^\w*[^\w]+$`, "", "ab_+-"},
		{`^(?<x>\d{2,})(?:-\d)?$`, "d", "123-4"},
		{`^.{0,3}$`, "s", "a\nb"},
		{`^[ab]+c`, "u", "abc\U0001f600"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, pattern, flags, subject string) {
		if len(pattern) > 64 || len(subject) > 64 {
			return
		}
		if _, ok := parseRegExpFlags(flags); !ok {
			return
		}
		checkRunPattern(t, NewRealm(), pattern, flags, FromGoString(subject))
	})
}

// FuzzRunPatternGen builds the pattern from the fuzzer's bytes out of
// runAtoms and runQuants (so most inputs are run patterns, or nearly) and
// compares it like FuzzRunPattern.
func FuzzRunPatternGen(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5}, "", "aab==")
	f.Add([]byte{4, 3, 2, 6, 0xff}, "i", "AZaz09+/=")
	f.Add([]byte{0x80, 11, 0, 12, 7}, "y", "ab1")
	f.Fuzz(func(t *testing.T, gen []byte, flags, subject string) {
		if len(gen) > 12 || len(subject) > 64 {
			return
		}
		if _, ok := parseRegExpFlags(flags); !ok {
			return
		}
		var b strings.Builder
		if len(gen) > 0 && gen[0]&0x80 == 0 {
			b.WriteByte('^')
		}
		for i := 1; i+1 < len(gen); i += 2 {
			b.WriteString(runAtoms[int(gen[i])%len(runAtoms)])
			b.WriteString(runQuants[int(gen[i+1])%len(runQuants)])
		}
		if len(gen) > 0 && gen[0]&0x40 != 0 {
			b.WriteByte('$')
		}
		checkRunPattern(t, NewRealm(), b.String(), flags, FromGoString(subject))
	})
}

// TestRunPatternSelection checks which patterns become run patterns.
func TestRunPatternSelection(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct {
		pattern, flags string
		run            bool
	}{
		{`^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$`, "", true},
		{`^https?:\/\/`, "i", true},
		{`^[a-z]+[a-z0-9]$`, "", false},    // the run could give back a letter
		{`^[a-z]*[0-9]*[a-z]$`, "", false}, // through a run that can match nothing
		{`^a+?b$`, "", false},              // lazy
		{`^a{2}?b$`, "", true},             // a fixed count is not lazy
		{`[a-z]+=`, "", false},             // unanchored
		{`[a-z]+=`, "y", true},
		{`^a|b$`, "", false},
		{`^(a)+$`, "", false},
		{`^a$`, "m", false},
		{`^\bab$`, "", false},
		{`^a(?=b)`, "", false},
		{`^(a)\1$`, "", false},
		{`^([a-z]+)(=*)$`, "", true},
	} {
		fl, ok := parseRegExpFlags(c.flags)
		require.True(t, ok)
		re, err := r.compileRegExp(FromGoString(c.pattern), fl)
		require.NoError(t, err)
		require.Equal(t, c.run, re.run != nil, "/%s/%s", c.pattern, c.flags)
	}
}

// TestRunPatternMethods runs a run pattern through every method that
// matches, with captures, named groups, indices, lastIndex and the legacy
// statics.
func TestRunPatternMethods(t *testing.T) {
	img := "data:image/png;base64," + strings.Repeat("iVBORw0KGgo", 3) + "="
	cases := []struct{ src, want string }{
		{`/^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$/.test(IMG)`, "true"},
		{`/^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$/.test(IMG + "x")`, "false"},
		{`/^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$/.test(IMG.replace("i", "é"))`, "false"},
		{`const m = /^data:(?<mime>image\/[a-z0-9.+-]+);base64,([A-Za-z0-9+/]+={0,2})$/d.exec(IMG); [m.index, m[0].length, m.groups.mime, m[2].length, m.indices[1].join(), m.indices.groups.mime.join(), RegExp.$1, RegExp.lastMatch.length, RegExp.leftContext, RegExp.rightContext].join("|")`,
			"0|56|image/png|34|5,14|5,14|image/png|56||"},
		{`IMG.match(/^data:([a-z]+)\/([a-z]+);/).slice(1).join()`, "image,png"},
		{`IMG.replace(/^data:[a-z]+\/[a-z]+;base64,/, "")`, strings.Repeat("iVBORw0KGgo", 3) + "="},
		{`IMG.replace(/^(data):([a-z]+)/, "$2:$1")`, "image:data" + img[10:]},
		{`IMG.search(/^data:/) + "," + IMG.search(/^image/)`, "0,-1"},
		{`"ab=cd".split(/^ab=/).join("|")`, "|cd"},
		{`const re = /^a+/g; const s = "aab"; [re.exec(s)[0], re.lastIndex, re.exec(s), re.lastIndex].join("|")`, "aa|2||0"},
		{`const re = /[a-z]+=/y; re.lastIndex = 2; const s = "ababc=d"; [re.exec(s)[0], re.lastIndex, re.exec(s), re.lastIndex].join("|")`, "abc=|6||0"},
		{`[..."aab".matchAll(/^a(a)/g)].map(m => m[0] + m[1] + m.index).join()`, "aaa0"},
		{`/^https?:\/\//i.test("HTTPS://x") + "," + /^https?:\/\//i.test("ftp://x")`, "true,false"},
		{`/^[a-z]+$/u.test("abc") + "," + /^.+$/s.test("a\nb") + "," + /^.+$/.test("a\nb")`, "true,true,false"},
		{`/^(a)(b)(c)(d)(e)(f)(g)(h=*)$/.exec("abcdefgh==").slice(1).join("")`, "abcdefgh=="},
	}
	for _, c := range cases {
		require.Equal(t, c.want, runScripts(t, `const IMG = "`+img+`"; `+c.src), c.src)
	}
}

// TestRunPatternInterrupt interrupts a run over a long subject.
func TestRunPatternInterrupt(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`^[A-Za-z0-9+/]+={0,2}$`), regexpFlags{})
	require.NoError(t, err)
	require.NotNil(t, c.run)
	text := []byte(strings.Repeat("QUJD", runChunk))
	r.Interrupt("stop")
	_, err = c.run.test(r, text)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
	ok, err := c.run.test(r, text)
	require.NoError(t, err)
	require.True(t, ok)
	// A run shorter than a chunk does not look.
	r.Interrupt("stop")
	ok, err = c.run.test(r, text[:runChunk-1])
	require.NoError(t, err)
	require.True(t, ok)
	r.ClearInterrupt()
}

// benchDataURL is a data URL of n base64 bytes, varied like an image's.
func benchDataURL(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte("data:image/png;base64,")
	x := uint64(88172645463325252)
	for range n {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b = append(b, alphabet[x&63])
	}
	return string(append(b, '='))
}

// BenchmarkRegExpDataURL validates a 1.5 MiB base64 data URL with the
// pattern plugins run over every image of a request (doubao's), by test and
// by exec with captures.
func BenchmarkRegExpDataURL(b *testing.B) {
	r := NewRealm()
	img := StringValue(FromGoString(benchDataURL(3 << 19)))
	for _, c := range []struct{ name, pattern, method string }{
		{"test", `^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$`, "test"},
		{"exec", `^data:(image\/[a-z0-9.+-]+);base64,([A-Za-z0-9+/]+={0,2})$`, "exec"},
	} {
		b.Run(c.name, func(b *testing.B) {
			rx, err := r.NewRegExp(FromGoString(c.pattern), EmptyString())
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(img.AsString().Len()))
			for b.Loop() {
				v, err := callMethodErr(r, ObjectValue(rx), c.method, img)
				if err != nil || v.IsNull() || v.IsBool() && !v.AsBool() {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestRunPatternFailAllocs checks that a failed match allocates nothing, as
// RE2's does not: plugins replace with anchored patterns that mostly miss.
func TestRunPatternFailAllocs(t *testing.T) {
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(`^(\s+)(x?)`), regexpFlags{})
	require.NoError(t, err)
	require.NotNil(t, c.run)
	var sub reSubject
	r.initRegExpSubject(&sub, FromGoString("no leading space"), c)
	allocs := testing.AllocsPerRun(100, func() {
		if m, _ := c.matchAt(r, &sub, 0, false); m != nil {
			t.Fatal("matched")
		}
	})
	require.Zero(t, allocs)
}

// TestRunPatternMemory bounds what a run pattern costs: nothing for a
// pattern that is not anchored, its literal's bytes for a long literal
// (not a table per character), one table per distinct set of more than one
// byte, and no run pattern past runMaxSteps or runMaxTables.
func TestRunPatternMemory(t *testing.T) {
	lit := strings.Repeat("abcdefghij", 1000)
	parse := func(pattern string, flags regexpFlags) *reAST {
		ast, err := parseRegExp(FromGoString(pattern).UTF16(), flags)
		require.NoError(t, err)
		return ast
	}
	unanchored := parse(lit, regexpFlags{})
	require.Zero(t, testing.AllocsPerRun(10, func() {
		if compileRunPattern(unanchored, regexpFlags{}) != nil {
			t.Fatal("unanchored run pattern")
		}
	}))

	anchored := parse("^"+lit+"=*$", regexpFlags{})
	p := compileRunPattern(anchored, regexpFlags{})
	require.NotNil(t, p)
	require.Len(t, p.steps, 2) // the literal and the run of '='
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const n = 50
	keep := make([]*runPattern, n)
	for i := range keep {
		keep[i] = compileRunPattern(anchored, regexpFlags{})
	}
	runtime.ReadMemStats(&after)
	perCompile := float64(after.TotalAlloc-before.TotalAlloc) / n
	// The literal grows by doubling while it is collected: transient bytes
	// proportional to the pattern (the old tables were 256 per character).
	require.Less(t, perCompile, float64(8*len(lit)+4096), "bytes allocated per compile")
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range keep {
		keep[i] = compileRunPattern(anchored, regexpFlags{})
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := (float64(after.HeapAlloc) - float64(before.HeapAlloc)) / n
	require.Less(t, retained, float64(len(lit)+2048), "bytes retained per pattern")
	runtime.KeepAlive(keep)

	// One table for the repeated set, shared single-byte tables.
	p = compileRunPattern(parse(`^[a-z]+-[a-z]+-[a-z]+=*-?\d{3}\d$`, regexpFlags{}), regexpFlags{})
	require.NotNil(t, p)
	tables := map[*[256]uint8]bool{}
	for _, st := range p.steps {
		if st.set != nil {
			tables[st.set] = true
		}
	}
	require.Len(t, tables, 4) // [a-z], '=', \d, and '-' is in the literals but '-?' is the shared '-' table
	require.Same(t, runByteTable('='), compileRunPattern(parse(`^a=+$`, regexpFlags{}), regexpFlags{}).steps[1].set)

	// Past the bounds: no run pattern (the other engines run it).
	require.Nil(t, compileRunPattern(parse("^"+strings.Repeat("[ab]x", runMaxSteps), regexpFlags{}), regexpFlags{}))
	var many strings.Builder
	many.WriteByte('^')
	for i := range runMaxTables + 1 {
		fmt.Fprintf(&many, "[%c%c]", 'a'+i, 'A'+i)
	}
	require.Nil(t, compileRunPattern(parse(many.String(), regexpFlags{}), regexpFlags{}))
}

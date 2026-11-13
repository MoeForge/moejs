package engine

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"time"
)

// fuzzRegExpMalformed reports the syntax errors that the u-mode grammar and
// the Annex B grammar share: a trailing backslash, an unterminated class,
// unbalanced parentheses outside classes, and a quantifier with nothing to
// repeat at the start of the pattern, of an alternative or of a group.
func fuzzRegExpMalformed(p []uint16) bool {
	depth, inClass, altStart, afterParen := 0, false, true, false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !inClass && altStart && (c == '*' || c == '+' || c == '?' && !afterParen || c == '{' && fuzzBraceQuantifier(p[i:])) {
			return true
		}
		altStart, afterParen = false, false
		switch {
		case c == '\\':
			if i+1 == len(p) {
				return true
			}
			i++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '(':
			depth++
			altStart, afterParen = true, true
		case c == ')':
			if depth--; depth < 0 {
				return true
			}
		case c == '|':
			altStart = true
		}
	}
	return inClass || depth != 0
}

// fuzzBraceQuantifier reports whether p starts with {n}, {n,} or {n,m}.
func fuzzBraceQuantifier(p []uint16) bool {
	i, digits := 1, 0
	scan := func() {
		for digits = 0; i < len(p) && p[i] >= '0' && p[i] <= '9'; i++ {
			digits++
		}
	}
	if scan(); digits == 0 {
		return false
	}
	if i < len(p) && p[i] == ',' {
		i++
		scan()
	}
	return i < len(p) && p[i] == '}'
}

// fuzzIsPair reports whether hi, lo is a surrogate pair.
func fuzzIsPair(hi, lo uint16) bool {
	return hi >= 0xD800 && hi < 0xDC00 && lo >= 0xDC00 && lo < 0xE000
}

// fuzzAdvance is AdvanceStringIndex.
func fuzzAdvance(s *String, i int, unicode bool) int {
	if unicode && i+1 < s.Len() && fuzzIsPair(s.At(i), s.At(i+1)) {
		return i + 2
	}
	return i + 1
}

// fuzzSplitsPair reports whether unit index i falls between the halves of a
// surrogate pair of s.
func fuzzSplitsPair(s *String, i int) bool {
	return i > 0 && i < s.Len() && fuzzIsPair(s.At(i-1), s.At(i))
}

// fuzzMatchAll is the spec's global @@match loop over RegExpBuiltinExec.
func fuzzMatchAll(t *testing.T, r *Realm, rx *Object, d *RegExpData, s *String) any {
	t.Helper()
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	if err := r.setRegExpLastIndex(rx, 0); err != nil {
		t.Fatal(err)
	}
	var out []any
	for {
		m, err := r.regexpBuiltinExec(rx, d, s, &sub)
		if err != nil {
			fuzzExecFailed(t, "exec", err)
		}
		if m == nil {
			break
		}
		out = append(out, s.Substring(m[0], m[1]).GoString())
		if m[0] == m[1] {
			if err := r.setRegExpLastIndex(rx, fuzzAdvance(s, m[1], d.c.flags.unicode)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out == nil {
		return nil
	}
	return out
}

// fuzzExecFailed fails t on a matcher error, except that an input the
// backtracking VM does not finish (the watchdog's interrupt, or its backtrack
// stack limit) is skipped: those limits are the designed outcome there.
func fuzzExecFailed(t *testing.T, what string, err error) {
	t.Helper()
	var ie *InterruptedError
	if errors.As(err, &ie) || strings.Contains(auditBErrName(err), btStackMessage) {
		t.Skip("backtracking VM limit")
	}
	t.Fatalf("%s: %v", what, err)
}

// FuzzRegExp checks the pattern parser, the RE2 emitter and the matchers.
// Parsing never panics and fails only with the two documented error kinds
// (and must fail on the malformed shapes above), and a parsed pattern always
// compiles. An exact pattern (reExact) emits an RE2 form; an emitted form
// parses in its largest search variant unless it hits an RE2 size limit,
// compiles, and has exactly the JavaScript capture groups; the pattern runs
// on RE2 exactly when it is exact and its form compiles, and on the
// backtracking VM otherwise. On the fuzzed subject, exec indices are in range
// (and, in u mode, never split a surrogate pair), a global match equals the
// spec's exec loop, and the single-class matcher agrees with the program
// without it on match, replace and test.
func FuzzRegExp(f *testing.F) {
	unesc := strings.NewReplacer("~u", `\u`)
	subjects := []string{"", "aAbB 12_\t\n-x", "Привет мир 😀 ab😀c", "foo.bar@baz\r\nqux"}
	for i, p := range []string{
		``, `a`, `a|b|`, `(a)|b`, `^\s+|\s+$`, `\bfoo\b`, `\B.`, `(?:a|b)*?c`, `a{2,3}`, `a{2,}?`, `x{`, `{`, `a{,5}`,
		`[a-z\d_]+`, `[^\s\S]`, `[]`, `[^]`, `[\b\-\]]`, `[a-]`, `[\w-z]`, `\d+(?=px)`, `(?!x)y`, `(?<=a)b`, `(?<!a)b`,
		`(?<year>\d{4})-(?<m>\d\d)\k<m>`, `\k<x>`, `(a)\1\2`, `\01\8`, `\cA\c1\c`, `\x4g\x41`, `~u{1F600}`, `~uD83D~uDE00`,
		`~uD83D`, `[~uD83D-~uDBFF]`, `\p{L}+\P{Nd}`, `\p{Script=Greek}`, `\p{Foo}`, `.`, `(?:)`, `()`, `(`, `)`, `a)`, `[`,
		`\`, `*`, `a**`, `+a`, `a|*`, `(*)`, `(?)`, `(?:a)+`, `a{1001}`, `(a{1000}){1000}`, `((((((((((a))))))))))`,
		`$^`, `\s\S\w\W\d\D`, `\/\.\*`, `\q`, `[\q]`, `\u`, `\x`, `a+?b??c*?`, `(?i:a)`, `(?<a>x)|(?<a>y)`,
		`\0`, `[\0-\x1f]{2}`, `\s+`, `[ ,;]+`, `\S{2,4}`, `(a+)+(?=b)`, `(?<=\$)\d+`, `(a)|\1b`, `(?:a?)*b`,
		`[\s--1]`, `[\d-x]+`, `[\p{L}--[a-z]]`, `[\q{abc|ab}x]`, `^$|\r`,
	} {
		p = unesc.Replace(p)
		for _, fl := range []string{"", "g", "i", "u", "gimsuyd", "gy", "gv", "x", "gg"} {
			f.Add(p, fl, subjects[i%len(subjects)])
		}
	}
	limits := []syntax.ErrorCode{syntax.ErrLarge, syntax.ErrNestingDepth, syntax.ErrInvalidRepeatSize}
	f.Fuzz(func(t *testing.T, pattern, flags, subject string) {
		if len(pattern) > 256 || len(subject) > 256 {
			return
		}
		fl, ok := parseRegExpFlags(flags)
		if !ok {
			return
		}
		units := FromGoString(pattern).UTF16()
		// A fresh realm per input: the watchdog's interrupt must not reach the
		// next one.
		r := NewRealm()
		mustReject := func(why string) {
			t.Helper()
			if _, err := r.NewRegExp(FromGoString(pattern), FromGoString(flags)); !strings.HasPrefix(auditBErrName(err), "SyntaxError:") {
				t.Fatalf("/%s/%s: %s, but NewRegExp gives %v", pattern, flags, why, err)
			}
		}
		ast, err := parseRegExp(units, fl)
		if err != nil {
			switch err.(type) {
			case *regexpUnsupportedError, *regexpInvalidError:
			default:
				t.Fatalf("/%s/%s: error is %T, want *regexpUnsupportedError or *regexpInvalidError: %v", pattern, flags, err, err)
			}
			mustReject("parsing fails")
			return
		}
		if fuzzRegExpMalformed(units) {
			t.Fatalf("/%s/%s is malformed but parses", pattern, flags)
		}
		onRE2 := false
		if tr, err := emitRE2(ast, fl, len(units)); err != nil {
			if _, ok := err.(*regexpUnsupportedError); !ok || reExact(ast.Root) {
				t.Fatalf("/%s/%s: emitting RE2 fails with %T: %v", pattern, flags, err, err)
			}
		} else if _, err := syntax.Parse(variantOpenLargest+tr.body+`)`, syntax.Perl); err != nil {
			var se *syntax.Error
			if !errors.As(err, &se) || !slices.Contains(limits, se.Code) {
				t.Fatalf("/%s/%s translates to %q, which RE2 rejects: %v", pattern, flags, tr.body, err)
			}
		} else {
			re, err := regexp.Compile(tr.body)
			if err != nil {
				t.Fatalf("/%s/%s: %q parses wrapped but does not compile: %v", pattern, flags, tr.body, err)
			}
			if n := len(tr.names) - 1; re.NumSubexp() != n {
				t.Fatalf("/%s/%s: %q has %d groups, the pattern %d", pattern, flags, tr.body, re.NumSubexp(), n)
			}
			onRE2 = reExact(ast.Root)
		}
		rx, err := r.NewRegExp(FromGoString(pattern), FromGoString(flags))
		if err != nil {
			t.Fatalf("/%s/%s parses but NewRegExp fails: %v", pattern, flags, err)
		}
		if d := rx.RegExpData(); (d.c.exact) != onRE2 {
			t.Fatalf("/%s/%s: runs on RE2 = %v, want %v", pattern, flags, d.c.exact, onRE2)
		}
		watchdog := time.AfterFunc(time.Second, func() { r.Interrupt("fuzz watchdog") })
		defer watchdog.Stop()
		d := rx.RegExpData()
		s := FromGoString(subject)

		var sub reSubject
		r.initRegExpSubject(&sub, s, d.c)
		if err := r.setRegExpLastIndex(rx, 0); err != nil {
			t.Fatal(err)
		}
		m, err := r.regexpBuiltinExec(rx, d, s, &sub)
		if err != nil {
			fuzzExecFailed(t, "exec", err)
		}
		if m != nil {
			if len(m) != 2*len(ast.Names) {
				t.Fatalf("/%s/%s on %q: %d indices for %d groups", pattern, flags, subject, len(m), len(ast.Names)-1)
			}
			for i := 0; i < len(m); i += 2 {
				if i > 0 && m[i] == -1 && m[i+1] == -1 {
					continue
				}
				if m[i] < 0 || m[i] > m[i+1] || m[i+1] > s.Len() || fl.unicode && (fuzzSplitsPair(s, m[i]) || fuzzSplitsPair(s, m[i+1])) {
					t.Fatalf("/%s/%s on %q: group %d at [%d, %d) of %d units", pattern, flags, subject, i/2, m[i], m[i+1], s.Len())
				}
			}
		}
		match := func(rx *Object, d *RegExpData) any {
			v, err := regexpMatch(r, rx, d, s)
			if err != nil {
				fuzzExecFailed(t, "match", err)
			}
			return r.ToGo(v)
		}
		if fl.global {
			if got, want := match(rx, d), fuzzMatchAll(t, r, rx, d, s); !slices.Equal(fuzzStrings(got), fuzzStrings(want)) {
				t.Fatalf("/%s/%s on %q: match = %q, exec loop = %q", pattern, flags, subject, got, want)
			}
		}
		if _, err := regexpSplit(r, d, s, 1<<32-1); err != nil {
			fuzzExecFailed(t, "split", err)
		}
		replace := func(rx *Object, d *RegExpData) any {
			if err := r.setRegExpLastIndex(rx, 0); err != nil {
				t.Fatal(err)
			}
			v, err := regexpReplace(r, rx, d, s, StringValue(FromGoString("[$&|$1|$`|$'|$<n>]")))
			if err != nil {
				fuzzExecFailed(t, "replace", err)
			}
			return r.ToGo(v)
		}
		got := replace(rx, d)
		if d.c.simple == nil {
			return
		}
		// The same program without the single-class matcher (RE2, or the VM
		// when RE2 rejected the form); the shared compiledRegExp itself is not
		// modified.
		rx2, err := r.NewRegExp(FromGoString(pattern), FromGoString(flags))
		if err != nil {
			t.Fatal(err)
		}
		d2 := rx2.RegExpData()
		d2.c = &compiledRegExp{tr: d.c.tr, flags: d.c.flags, key: d.c.key, exact: d.c.exact, btCR: d.c.btCR, btFoldWord: d.c.btFoldWord}
		if want := replace(rx2, d2); got != want {
			t.Fatalf("/%s/%s on %q: single-class replace = %q, without it %q", pattern, flags, subject, got, want)
		}
		for _, x := range []*Object{rx, rx2} {
			if err := r.setRegExpLastIndex(x, 0); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := fuzzStrings(match(rx, d)), fuzzStrings(match(rx2, d2)); !slices.Equal(got, want) {
			t.Fatalf("/%s/%s on %q: single-class match = %q, without it %q", pattern, flags, subject, got, want)
		}
		test := func(rx *Object) Value {
			if err := r.setRegExpLastIndex(rx, 0); err != nil {
				t.Fatal(err)
			}
			v, err := regexpProtoTest(r, ObjectValue(rx), []Value{StringValue(s)})
			if err != nil {
				fuzzExecFailed(t, "test", err)
			}
			return v
		}
		if got, want := test(rx), test(rx2); got != want {
			t.Fatalf("/%s/%s on %q: single-class test = %v, without it %v", pattern, flags, subject, got, want)
		}
	})
}

// fuzzStrings flattens a ToGo match result (nil or a list of strings and
// nils) for comparison.
func fuzzStrings(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs)+1)
	if v == nil {
		out = append(out, "<null>")
	}
	for _, x := range xs {
		s, ok := x.(string)
		if !ok {
			s = "<undefined>"
		}
		out = append(out, s)
	}
	return out
}

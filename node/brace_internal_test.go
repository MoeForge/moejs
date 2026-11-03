package node

import (
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// refExpand is the straightforward expansion: the first group's
// alternatives, each followed by the rest, expanded again.
func refExpand(p string) []string {
	for i := 0; i < len(p); i++ {
		if p[i] != '{' {
			continue
		}
		close := refMatch(p, i)
		if close < 0 {
			continue
		}
		body := p[i+1 : close]
		alts := refSplit(body)
		if len(alts) < 2 {
			if strings.ContainsAny(body, "{}") {
				continue
			}
			if alts = refRange(body); alts == nil {
				continue
			}
		}
		var out []string
		for _, a := range alts {
			for _, rest := range refExpand(a + p[close+1:]) {
				out = append(out, p[:i]+rest)
			}
		}
		return out
	}
	return []string{p}
}

func refMatch(p string, i int) int {
	depth := 0
	for j := i; j < len(p); j++ {
		switch p[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

func refSplit(body string) []string {
	var out []string
	depth, start := 0, 0
	for j := 0; j < len(body); j++ {
		switch body[j] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:j])
				start = j + 1
			}
		}
	}
	return append(out, body[start:])
}

func refRange(body string) []string {
	parts := strings.Split(body, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return nil
	}
	step := 1
	if len(parts) == 3 {
		s, err := strconv.Atoi(parts[2])
		if err != nil || s == 0 {
			return nil
		}
		step = max(s, -s)
	}
	a, errA := strconv.Atoi(parts[0])
	b, errB := strconv.Atoi(parts[1])
	var out []string
	if errA == nil && errB == nil {
		width := 0
		if (len(parts[0]) > 1 && strings.TrimLeft(parts[0], "-")[0] == '0') ||
			(len(parts[1]) > 1 && strings.TrimLeft(parts[1], "-")[0] == '0') {
			width = max(len(parts[0]), len(parts[1]))
		}
		for i := a; ; {
			s := strconv.Itoa(i)
			if neg := strings.HasPrefix(s, "-"); width > 0 && len(s) < width {
				z := strings.Repeat("0", width-len(s))
				if neg {
					s = "-" + z + s[1:]
				} else {
					s = z + s
				}
			}
			out = append(out, s)
			if i == b {
				return out
			}
			if a < b {
				if i += step; i > b {
					return out
				}
			} else if i -= step; i < b {
				return out
			}
		}
	}
	if len(parts[0]) == 1 && len(parts[1]) == 1 {
		x, y := int(parts[0][0]), int(parts[1][0])
		for i := x; ; {
			out = append(out, string(rune(i)))
			if i == y {
				return out
			}
			if x < y {
				if i += step; i > y {
					return out
				}
			} else if i -= step; i < y {
				return out
			}
		}
	}
	return nil
}

// TestBraceExpansion compares the expansion with refExpand, on fixed
// patterns and on random ones made of braces, commas, sequences and
// letters.
func TestBraceExpansion(t *testing.T) {
	check := func(p string) {
		t.Helper()
		got, err := expandBraces(p, nil)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		want := refExpand(p)
		if len(want) > maxExpansions {
			want = want[:maxExpansions]
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%q: got %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{
		"", "a", "{a,b}", "x{a,b}y", "{a,b}{c,d}", "a{b,c{d,e}f}g", "{a}", "{a}{b,c}", "{{a,b}}", "{x{a,b}y}",
		"{,}", "{a,}", "{,b}c", "{1..3}", "{3..1}", "{01..3}", "{1..10..3}", "{10..1..-3}", "{-1..1}", "{-05..5..5}",
		"{a..e}", "{e..a..2}", "{a..5}", "{1..3}{a,b}", "{a,b}}", "{{a,b}", "}{a,b}{", "{a,{b,{c,d}}}e", "{..}", "{{..}}",
		"a/{b,c}/**/{d..f}.js",
	} {
		check(p)
	}
	// Steps and ends at the bounds of int64.
	for p, want := range map[string][]string{
		"{1..2..-9223372036854775808}": {"1"},
		"{-9223372036854775808..9223372036854775807..4611686018427387904}": {
			"-9223372036854775808", "-4611686018427387904", "0", "4611686018427387904"},
	} {
		if got, err := expandBraces(p, nil); err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: got %q, %v; want %q", p, got, err, want)
		}
	}
	toks := []string{"a", "b", "{", "}", ",", ",", "{a,b}", "{1..3}", "{c..a}", "..", "1", "0", "/", "{", "}", "{,}"}
	rnd := rand.New(rand.NewSource(1))
	for range 20000 {
		var b strings.Builder
		for n := rnd.Intn(10); n > 0; n-- {
			b.WriteString(toks[rnd.Intn(len(toks))])
		}
		check(b.String())
	}
}

// TestBraceBounds: braces whose expansion is long fail as soon as it
// passes maxExpandedLength, in about the time of what they expanded to;
// many that expand to empty patterns stop at maxExpansions, and braces
// that nest too deep fail.
func TestBraceBounds(t *testing.T) {
	cases := []struct {
		p    string
		err  error
		want int
	}{
		{strings.Repeat("{a,b}", 13000), errExpandsTooLong, 0},
		{strings.Repeat("{a,b}", 12) + strings.Repeat("x", 65000), errExpandsTooLong, 0},
		{strings.Repeat("{,}", 21000), nil, maxExpansions},
		{strings.Repeat("{a,", 1100) + strings.Repeat("}", 1100), errBracesTooDeep, 0},
		{strings.Repeat("{a,", 1000) + strings.Repeat("}", 1000), nil, 1001},
		{"{" + strings.Repeat("0", 60000) + "1..99}", errExpandsTooLong, 0},
		{"{1..100000000}", nil, maxExpansions},
		{strings.Repeat("{", 40000) + strings.Repeat("}", 20000), nil, 1},
	}
	for _, c := range cases {
		start := time.Now()
		charged := 0
		cc := &globCompile{charge: func(n int) error { charged += n; return nil }}
		got, err := expandBraces(c.p, cc)
		d := time.Since(start)
		if err != c.err || (err == nil && len(got) != c.want) {
			t.Errorf("%.40q: %d patterns, %v; want %d, %v", c.p, len(got), err, c.want, c.err)
		}
		if d > time.Second {
			t.Errorf("%.40q: took %v", c.p, d)
		}
		if charged < len(c.p) {
			t.Errorf("%.40q: charged %d", c.p, charged)
		}
	}
}

package node

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// refParseToks, refParseExt and refParseClass are the recursive parse of a
// segment, which reads the rest again for each "(" or "[" that does not
// close.
func refParseToks(s string, i int, inExt bool) ([]tok, int) {
	var toks []tok
	for i < len(s) {
		c := s[i]
		if inExt && (c == '|' || c == ')') {
			return toks, i
		}
		if (c == '@' || c == '?' || c == '*' || c == '+' || c == '!') && i+1 < len(s) && s[i+1] == '(' {
			if alts, end, ok := refParseExt(s, i+2); ok {
				toks = append(toks, tok{kind: tokExt, ext: c, alts: alts})
				i = end
				continue
			}
		}
		switch c {
		case '\\':
			if i+1 < len(s) {
				r, n := utf8.DecodeRuneInString(s[i+1:])
				toks = append(toks, tok{kind: tokLit, lit: string(r)})
				i += 1 + n
				continue
			}
		case '?':
			toks = append(toks, tok{kind: tokAny})
			i++
			continue
		case '*':
			if len(toks) == 0 || toks[len(toks)-1].kind != tokStar {
				toks = append(toks, tok{kind: tokStar})
			}
			i++
			continue
		case '[':
			if cls, end, ok := refParseClass(s, i+1); ok {
				toks = append(toks, tok{kind: tokClass, cls: cls})
				i = end
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		toks = append(toks, tok{kind: tokLit, lit: string(r)})
		i += n
	}
	return toks, i
}

func refParseExt(s string, i int) ([][]tok, int, bool) {
	var alts [][]tok
	for {
		toks, j := refParseToks(s, i, true)
		if j >= len(s) {
			return nil, 0, false
		}
		alts = append(alts, toks)
		if s[j] == ')' {
			return alts, j + 1, true
		}
		i = j + 1
	}
}

func refParseClass(s string, i int) (*charClass, int, bool) {
	c := &charClass{}
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		c.neg = true
		i++
	}
	first := true
	for i < len(s) {
		if s[i] == ']' && !first {
			return c, i + 1, true
		}
		first = false
		if s[i] == '[' && i+1 < len(s) && s[i+1] == ':' {
			if end := strings.Index(s[i+2:], ":]"); end >= 0 {
				if f, ok := posixClasses[s[i+2:i+2+end]]; ok {
					c.named = append(c.named, f)
					c.names = append(c.names, s[i+2:i+2+end])
					i += end + 4
					continue
				}
			}
		}
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		lo, n := utf8.DecodeRuneInString(s[i:])
		i += n
		hi := lo
		if i+1 < len(s) && s[i] == '-' && s[i+1] != ']' {
			i++
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			hi, n = utf8.DecodeRuneInString(s[i:])
			i += n
		}
		c.ranges = append(c.ranges, [2]rune{lo, hi})
	}
	return nil, 0, false
}

// tokString writes toks as a string, to compare parses.
func tokString(b *strings.Builder, toks []tok) {
	for _, t := range toks {
		switch t.kind {
		case tokLit:
			for _, r := range t.lit {
				fmt.Fprintf(b, "L%q", r)
			}
		case tokAny:
			b.WriteString("?")
		case tokStar:
			b.WriteString("*")
		case tokClass:
			fmt.Fprintf(b, "[%v %v %v]", t.cls.neg, t.cls.ranges, t.cls.names)
		case tokExt:
			fmt.Fprintf(b, "%c(", t.ext)
			for i, a := range t.alts {
				if i > 0 {
					b.WriteString("|")
				}
				tokString(b, a)
			}
			b.WriteString(")")
		}
	}
}

// TestSegmentParse compares the parse of segments with the recursive one,
// on random segments of extglobs, classes and the characters they are
// made of, and checks that segments of many "(" and "[" that do not
// close parse at once.
func TestSegmentParse(t *testing.T) {
	toks := []string{"a", "b", "@(", "?(", "*(", "+(", "!(", "(", ")", "|", "|", "[", "]", "[!", "[^", "[:alpha:]", "[:", ":]", "-", "*", "?", "x", "é"}
	rnd := rand.New(rand.NewSource(2))
	for range 50000 {
		var b strings.Builder
		for n := rnd.Intn(16); n > 0; n-- {
			b.WriteString(toks[rnd.Intn(len(toks))])
		}
		s := b.String()
		want, _ := refParseToks(s, 0, false)
		p, err := newSegParser(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.toks(0, len(s))
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		var gb, wb strings.Builder
		tokString(&gb, got)
		tokString(&wb, want)
		if gb.String() != wb.String() {
			t.Fatalf("%q:\n got %s\nwant %s", s, gb.String(), wb.String())
		}
	}
	for _, s := range []string{
		strings.Repeat("@(", 30000), strings.Repeat("@(a|", 20000), strings.Repeat("[", 60000), strings.Repeat("[[:alpha:]", 6000),
		strings.Repeat("[:", 30000), strings.Repeat("+(a[", 15000), strings.Repeat("@(a)", 15000),
	} {
		start := time.Now()
		p, err := newSegParser(s, nil)
		if err == nil {
			_, err = p.toks(0, len(s))
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("%.20q: %v", s, d)
		}
		if err != nil {
			t.Logf("%.20q: %v", s, err)
		}
	}
	s := strings.Repeat("@(", 300) + strings.Repeat(")", 300)
	p, _ := newSegParser(s, nil)
	if _, err := p.toks(0, len(s)); err != errTooComplex {
		t.Errorf("extglobs 300 deep: %v", err)
	}
}

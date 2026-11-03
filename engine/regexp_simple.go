package engine

import (
	"slices"
	"strings"
)

// Single-class regular expressions.
//
// The plugin corpus runs a handful of patterns per hook and most of them are
// one character class with a quantifier: /\s+/g, /\d+/, /[^a-z0-9]+/g. Go's
// regexp allocates the capture slice of every match, allocates the result
// list of FindAll, and needs a UTF-8 working copy plus two index maps for a
// UTF-16 subject. A pattern that is exactly one character class followed by
// at most one greedy quantifier is matched here instead, directly on the
// string's code units. The recognizer is deliberately narrow: anything it
// does not understand returns nil and the translated RE2 program is used,
// so the accepted subset is the only thing that has to be exact.
//
// Accepted: one literal character (not a syntax character or a surrogate)
// or an identity escape of a syntax character or '/', `\d \D \s \S \w \W`,
// or a bracket class of literal code units,
// ranges between literals, the class escapes above, `\t \n \v \f \r \0 \b`
// (backspace), `\xHH`, `\uHHHH` and identity escapes of syntax characters;
// followed by nothing, `+`, `{n}`, `{n,}` or `{n,m}` with n >= 1. Rejected:
// the i flag (case folding), lazy quantifiers, anything that can match the
// empty string, literal surrogate code units in the class (their per-unit
// versus per-code-point treatment differs between modes), and in u mode
// negated classes and classes whose ranges reach into the surrogate area
// (an astral code point is one character there, outside any BMP range, but
// its two halves are units this matcher would see).

// unitSet is a set of BMP code units: an ASCII bitmap plus sorted,
// non-overlapping ranges above 0x7F, optionally complemented.
type unitSet struct {
	ascii  [2]uint64
	ranges []unitRange
	negate bool
}

type unitRange struct{ lo, hi uint16 }

func (s *unitSet) addUnit(c uint16) { s.addRange(c, c) }

func (s *unitSet) addRange(lo, hi uint16) {
	for c := int(lo); c <= int(hi) && c < 0x80; c++ {
		s.ascii[c>>6] |= 1 << (c & 63)
	}
	if hi >= 0x80 {
		s.ranges = append(s.ranges, unitRange{max(lo, 0x80), hi})
	}
}

// addEscape adds the set of a class escape letter (d, s, w and complements).
func (s *unitSet) addEscape(c uint16) bool {
	switch c {
	case 'd':
		s.addRange('0', '9')
	case 'w':
		s.addRange('0', '9')
		s.addRange('A', 'Z')
		s.addRange('a', 'z')
		s.addUnit('_')
	case 's':
		for _, c := range [...]uint16{'\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF} {
			s.addUnit(c)
		}
		s.addRange(0x2000, 0x200A)
	case 'D', 'S', 'W':
		// A complemented escape inside a class would need set subtraction;
		// standalone it is handled by negate in compileSimpleClass.
		return false
	default:
		return false
	}
	return true
}

// finish sorts and merges the ranges.
func (s *unitSet) finish() {
	slices.SortFunc(s.ranges, func(a, b unitRange) int { return int(a.lo) - int(b.lo) })
	out := s.ranges[:0]
	for _, r := range s.ranges {
		if n := len(out); n > 0 && int(r.lo) <= int(out[n-1].hi)+1 {
			out[n-1].hi = max(out[n-1].hi, r.hi)
			continue
		}
		out = append(out, r)
	}
	s.ranges = out
}

// hasSurrogates reports whether a range covers a surrogate code unit
// (after finish).
func (s *unitSet) hasSurrogates() bool {
	for _, r := range s.ranges {
		if r.hi >= 0xD800 && r.lo < 0xE000 {
			return true
		}
	}
	return false
}

func (s *unitSet) has(c uint16) bool {
	var in bool
	if c < 0x80 {
		in = s.ascii[c>>6]&(1<<(c&63)) != 0
	} else {
		in = s.hasRange(c)
	}
	return in != s.negate
}

func (s *unitSet) hasRange(c uint16) bool {
	lo, hi := 0, len(s.ranges)
	for lo < hi {
		m := (lo + hi) >> 1
		switch r := s.ranges[m]; {
		case c < r.lo:
			hi = m
		case c > r.hi:
			lo = m + 1
		default:
			return true
		}
	}
	return false
}

// simpleClass is a compiled single-class pattern: a run of min..max units
// of set (max < 0 means unbounded).
type simpleClass struct {
	set      unitSet
	min, max int
	one      int32 // the unit of a one-character pattern, else -1
}

// compileSimpleClass recognizes pattern (code units) with flags, or returns
// nil when the pattern is outside the accepted subset (which excludes the v
// flag's class syntax).
func compileSimpleClass(pattern []uint16, flags regexpFlags) *simpleClass {
	if flags.ignoreCase || flags.unicodeSets || len(pattern) == 0 {
		return nil
	}
	p := simpleParser{src: pattern, unicode: flags.unicode, one: -1}
	set, ok := p.class()
	if !ok {
		return nil
	}
	c := &simpleClass{set: set, min: 1, max: 1, one: p.one}
	if p.pos < len(p.src) {
		if !p.quantifier(c) {
			return nil
		}
	}
	if p.pos != len(p.src) || c.min < 1 {
		return nil
	}
	c.set.finish()
	if flags.unicode && (c.set.negate || c.set.hasSurrogates()) {
		return nil
	}
	return c
}

type simpleParser struct {
	src     []uint16
	pos     int
	unicode bool
	one     int32 // the unit when the class is one literal character
}

func (p *simpleParser) peek() (uint16, bool) {
	if p.pos >= len(p.src) {
		return 0, false
	}
	return p.src[p.pos], true
}

// class parses the leading class atom.
func (p *simpleParser) class() (unitSet, bool) {
	var set unitSet
	if c, ok := p.literal(); ok {
		set.addUnit(c)
		p.one = int32(c)
		return set, true
	}
	c, _ := p.peek()
	if c == '\\' {
		if p.pos+1 >= len(p.src) {
			return set, false
		}
		e := p.src[p.pos+1]
		p.pos += 2
		switch e {
		case 'D', 'S', 'W':
			set.negate = true
			e += 'a' - 'A'
		}
		return set, set.addEscape(e)
	}
	if c != '[' {
		return set, false
	}
	p.pos++
	if c, ok := p.peek(); ok && c == '^' {
		set.negate = true
		p.pos++
	}
	for {
		c, ok := p.peek()
		if !ok {
			return set, false
		}
		if c == ']' {
			p.pos++
			return set, true
		}
		lo, escape, ok := p.classAtom(&set)
		if !ok {
			return set, false
		}
		if escape {
			// Annex B: `\s-x` is the union of \s, '-' and the atom x, which
			// cannot start a range (`[\s--1]` has no '0'); a '-' before ']'
			// is left to the next iteration as a literal.
			if n, ok := p.peek(); ok && n == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] != ']' {
				p.pos++
				set.addUnit('-')
				x, escape, ok := p.classAtom(&set)
				if !ok {
					return set, false
				}
				if !escape {
					set.addUnit(x)
				}
			}
			continue
		}
		// A range lo-hi between two literals; a '-' before ']' is literal.
		if n, ok := p.peek(); ok && n == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] != ']' {
			p.pos++
			hi, escape, ok := p.classAtom(&set)
			if !ok || escape || hi < lo {
				return set, false
			}
			set.addRange(lo, hi)
			continue
		}
		set.addUnit(lo)
	}
}

// literal consumes one literal character (a set of one unit): a character
// that is not a syntax character or a surrogate, or an identity escape of a
// syntax character or '/'.
func (p *simpleParser) literal() (uint16, bool) {
	c, ok := p.peek()
	if !ok {
		return 0, false
	}
	n := 1
	switch c {
	case '\\':
		if p.pos+1 >= len(p.src) {
			return 0, false
		}
		switch c = p.src[p.pos+1]; c {
		case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '/':
			n = 2
		default:
			return 0, false
		}
	case '^', '$', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|':
		return 0, false
	}
	if c >= 0xD800 && c < 0xE000 {
		return 0, false
	}
	p.pos += n
	return c, true
}

// classAtom parses one class atom. A class escape (\d \s \w) is added to
// set directly and reported through escape; a literal unit is returned.
func (p *simpleParser) classAtom(set *unitSet) (unit uint16, escape bool, ok bool) {
	c := p.src[p.pos]
	p.pos++
	if c >= 0xD800 && c < 0xE000 {
		return 0, false, false
	}
	if c != '\\' {
		return c, false, true
	}
	e, present := p.peek()
	if !present {
		return 0, false, false
	}
	p.pos++
	switch e {
	case 'd', 's', 'w':
		return 0, true, set.addEscape(e)
	case 't':
		return '\t', false, true
	case 'n':
		return '\n', false, true
	case 'v':
		return '\v', false, true
	case 'f':
		return '\f', false, true
	case 'r':
		return '\r', false, true
	case 'b':
		return '\b', false, true
	case '0':
		if n, ok := p.peek(); ok && n >= '0' && n <= '9' {
			return 0, false, false // legacy octal
		}
		return 0, false, true
	case 'x':
		v, ok := p.hex(2)
		return v, false, ok
	case 'u':
		v, ok := p.hex(4)
		if !ok || (v >= 0xD800 && v < 0xE000) {
			return 0, false, false
		}
		return v, false, true
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '/', '-':
		return e, false, true
	}
	return 0, false, false
}

func (p *simpleParser) hex(n int) (uint16, bool) {
	if p.pos+n > len(p.src) {
		return 0, false
	}
	var v uint16
	for range n {
		d := digitValueUnit(p.src[p.pos])
		if d < 0 || d >= 16 {
			return 0, false
		}
		v = v<<4 | uint16(d)
		p.pos++
	}
	return v, true
}

// quantifier parses `+`, `{n}`, `{n,}` or `{n,m}` (greedy) into c.
func (p *simpleParser) quantifier(c *simpleClass) bool {
	switch p.src[p.pos] {
	case '+':
		p.pos++
		c.min, c.max = 1, -1
	case '{':
		p.pos++
		lo, ok := p.decimal()
		if !ok {
			return false
		}
		c.min, c.max = lo, lo
		if n, ok := p.peek(); ok && n == ',' {
			p.pos++
			if n, ok := p.peek(); ok && n == '}' {
				c.max = -1
			} else if hi, ok := p.decimal(); ok && hi >= lo {
				c.max = hi
			} else {
				return false
			}
		}
		if n, ok := p.peek(); !ok || n != '}' {
			return false
		}
		p.pos++
	default:
		return false
	}
	if n, ok := p.peek(); ok && n == '?' {
		return false // lazy
	}
	return true
}

func (p *simpleParser) decimal() (int, bool) {
	start := p.pos
	v := 0
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		v = v*10 + int(p.src[p.pos]-'0')
		if v > 1000 {
			return 0, false // RE2's repeat limit; let the translator report it
		}
		p.pos++
	}
	return v, p.pos > start
}

// find returns the leftmost match of c in s at or after from as code-unit
// positions, or ok == false.
func (c *simpleClass) find(s *String, from int) (start, end int, ok bool) {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	n := int(f.n)
	i := from
	for i < n {
		// Skip to the first unit in the set: one character's with
		// IndexByte, as RE2 skips to a literal.
		if f.kind == strASCII && c.one >= 0 {
			k := -1
			if c.one < 0x80 {
				k = strings.IndexByte(f.s[i:], byte(c.one))
			}
			if k < 0 {
				return 0, 0, false
			}
			i += k
		} else if f.kind == strASCII {
			for i < n && !c.set.has(uint16(f.s[i])) {
				i++
			}
		} else {
			u := f.units()
			for i < n && !c.set.has(u[i]) {
				i++
			}
		}
		if i >= n {
			return 0, 0, false
		}
		// Extend the run, but no further than max: the caller resumes from
		// the match end, so scanning the whole run for a bounded quantifier
		// (/[a]/g on a run of a's) would revisit it once per match.
		limit := n
		if c.max >= 0 && c.max < n-i {
			limit = i + c.max
		}
		j := i + 1
		if f.kind == strASCII {
			for j < limit && c.set.has(uint16(f.s[j])) {
				j++
			}
		} else {
			u := f.units()
			for j < limit && c.set.has(u[j]) {
				j++
			}
		}
		if j-i >= c.min {
			return i, j, true
		}
		i = j // a shorter run cannot match from any position inside it
	}
	return 0, 0, false
}

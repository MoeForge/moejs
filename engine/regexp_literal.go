package engine

import (
	"bytes"
	"strings"
)

// Required literals. Many patterns can only match where a fixed string
// occurs: `/(^|[^\\])"\\\/Date\((-?\d+)\)/` matches nowhere in a subject
// without `"\/Date(`. Such a literal, found at compile time, lets a search
// fail on a string scan instead of a run of the matcher at every position,
// and lets it start late: a match contains the literal at most pre code
// units after its start, so none starts before the first occurrence minus
// pre. Only the matcher call is skipped or moved; lastIndex, the legacy
// statics and every other step of the callers run as they would after the
// matcher failed or found that match.
//
// The literal is a run of characters of the pattern's top-level sequence
// (looking into capture groups), each a set of one code unit or of an ASCII
// letter in both cases (the i flag): a match consumes them in a row. Sets
// of any other kind end a run, so the search finds exactly the strings
// those characters match, whatever the flags (with i and u, k and s also
// match U+212A and U+017F and are not literal letters). Alternatives,
// repeats, lookarounds and backreferences never contribute. A pattern
// anchored at the start (a leading ^ without m) has none: its matcher fails
// at once where the literal would, and a sticky search, which tries one
// position, is not prefiltered either.
//
// A pattern that is its literal and nothing else (whole: no group, and in u
// mode no lone surrogate, which matches only outside a pair) matches where
// the scan finds it, and runs no matcher at all.

// reLiteral is a string every match of a pattern contains.
type reLiteral struct {
	// units is the literal; a unit of a folded letter is lower case, and
	// fold marks it.
	units []uint16
	fold  []bool
	// text is units as a string when every unit is ASCII, else "": an
	// ASCII subject never contains the literal.
	text    string
	anyFold bool
	// key is the index of the unit a scan looks for first: the rarest
	// (unitRank) that is not folded, or 0.
	key int
	// pre is the most code units a match has before the literal, -1 when
	// unbounded.
	pre int
	// whole is set when a match is the literal and nothing else.
	whole bool
}

// reLiteralMaxPre bounds pre; a larger bound is treated as unbounded.
const reLiteralMaxPre = 1 << 24

// requiredLiteral returns the longest literal of ast's top-level sequence,
// or nil, as for a pattern anchored at the start.
func requiredLiteral(ast *reAST, flags regexpFlags) *reLiteral {
	var items []*reNode
	var flatten func(n *reNode)
	flatten = func(n *reNode) {
		switch {
		case n.Op == reOpSeq:
			for _, s := range n.Subs {
				flatten(s)
			}
		case n.Op == reOpCapture, n.Op == reOpRepeat && n.Min == 1 && n.Max == 1:
			flatten(n.Subs[0])
		default:
			items = append(items, n)
		}
	}
	flatten(ast.Root)
	if len(items) == 0 || items[0].Op == reOpBegin && !items[0].Multiline {
		return nil
	}
	bestAt, bestLen, runAt := 0, 0, 0
	for i, n := range items {
		if _, _, ok := literalUnit(n); !ok {
			runAt = i + 1
			continue
		}
		if i+1-runAt > bestLen {
			bestAt, bestLen = runAt, i+1-runAt
		}
	}
	if bestLen == 0 {
		return nil
	}
	l := &reLiteral{}
	lone := false // a lone surrogate of a u-mode pattern
	for _, n := range items[bestAt : bestAt+bestLen] {
		c, fold, _ := literalUnit(n)
		lone = lone || flags.unicode && isSurrogateUnit(c)
		if c > 0xFFFF {
			hi, lo := encodeSurrogates(c)
			l.units = append(l.units, hi, lo)
			l.fold = append(l.fold, false, false)
			continue
		}
		l.units = append(l.units, uint16(c))
		l.fold = append(l.fold, fold)
		l.anyFold = l.anyFold || fold
	}
	l.key = -1
	for i, f := range l.fold {
		if !f && (l.key < 0 || unitRank(l.units[i]) > unitRank(l.units[l.key])) {
			l.key = i
		}
	}
	l.key = max(l.key, 0)
	text := make([]byte, 0, len(l.units))
	for _, c := range l.units {
		if c >= 0x80 {
			text = nil
			break
		}
		text = append(text, byte(c))
	}
	l.text = string(text)
	for _, n := range items[:bestAt] {
		k := reMaxUnits(n, flags.unicode)
		if k < 0 || l.pre+k > reLiteralMaxPre {
			l.pre = -1
			break
		}
		l.pre += k
	}
	l.whole = bestLen == len(items) && len(ast.Names) == 1 && !lone
	return l
}

// literalUnit returns the character a node of a required literal matches:
// a set of one character, or of one ASCII letter in both cases (fold, the
// lower case letter returned).
func literalUnit(n *reNode) (c rune, fold, ok bool) {
	if n.Op != reOpChar {
		return 0, false, false
	}
	if c, ok := n.Set.Single(); ok {
		return c, false, true
	}
	s := n.Set
	if len(s) == 4 && s[0] == s[1] && s[2] == s[3] && s[0] >= 'A' && s[0] <= 'Z' && s[2] == s[0]+0x20 {
		return s[2], true, true
	}
	return 0, false, false
}

func encodeSurrogates(c rune) (hi, lo uint16) {
	c -= 0x10000
	return uint16(0xD800 + c>>10), uint16(0xDC00 + c&0x3FF)
}

// reMaxUnits returns the most code units n can consume, or -1 when
// unbounded. A character of a set past the BMP is two units in u mode.
func reMaxUnits(n *reNode, unicode bool) int {
	switch n.Op {
	case reOpChar:
		if unicode && len(n.Set) > 0 && n.Set[len(n.Set)-1] > 0xFFFF {
			return 2
		}
		return 1
	case reOpSeq:
		sum := 0
		for _, s := range n.Subs {
			k := reMaxUnits(s, unicode)
			if k < 0 || sum+k > reLiteralMaxPre {
				return -1
			}
			sum += k
		}
		return sum
	case reOpAlt:
		most := 0
		for _, s := range n.Subs {
			k := reMaxUnits(s, unicode)
			if k < 0 {
				return -1
			}
			most = max(most, k)
		}
		return most
	case reOpCapture:
		return reMaxUnits(n.Subs[0], unicode)
	case reOpRepeat:
		if n.Max == 0 {
			return 0
		}
		k := reMaxUnits(n.Subs[0], unicode)
		if k == 0 {
			return 0
		}
		if k < 0 || n.Max < 0 || n.Max > reLiteralMaxPre/k {
			return -1
		}
		return k * n.Max
	case reOpBackref:
		return -1
	}
	return 0 // empty, assertions, lookarounds
}

// start returns the position a search for a match at or after pos may
// start from instead, or ok false when no match is possible. A sticky
// search starts at pos.
func (l *reLiteral) start(sub *reSubject, pos int, sticky bool) (int, bool) {
	if sticky {
		return pos, true
	}
	u := sub.toUnit(pos)
	at := l.index(sub, u, sub.s.Len())
	if at < 0 {
		return 0, false
	}
	if l.pre < 0 || at-l.pre <= u {
		return pos, true
	}
	s := at - l.pre
	// In u mode a match never starts inside a surrogate pair (the low half
	// is no character boundary, and from the high half the literal would be
	// past pre).
	if sub.unicode && sub.wide != nil && isLowSurrogate(rune(sub.wide[s])) && isHighSurrogate(rune(sub.wide[s-1])) {
		s++
	}
	return sub.toByte(s), true
}

// index returns the first code unit index in [from, to) where the literal
// starts and ends before to, or -1.
func (l *reLiteral) index(sub *reSubject, from, to int) int {
	if to-from < len(l.units) {
		return -1
	}
	if sub.wide == nil {
		if l.text == "" {
			return -1
		}
		return l.indexASCII(bytesToString(sub.text[:to]), from)
	}
	return l.indexUnits(sub.wide[:to], from)
}

func (l *reLiteral) indexASCII(s string, from int) int {
	if !l.anyFold {
		if i := strings.Index(s[from:], l.text); i >= 0 {
			return from + i
		}
		return -1
	}
	k, kc := l.key, byte(l.units[l.key])
	last := len(s) - len(l.units) + k // the last index of the key unit
	for i := from + k; i <= last; i++ {
		if l.fold[k] {
			for s[i]|0x20 != kc {
				if i++; i > last {
					return -1
				}
			}
		} else {
			j := strings.IndexByte(s[i:last+1], kc)
			if j < 0 {
				return -1
			}
			i += j
		}
		if l.equalASCII(s[i-k:]) {
			return i - k
		}
	}
	return -1
}

func (l *reLiteral) equalASCII(s string) bool {
	for i, c := range l.units {
		b := s[i]
		if l.fold[i] {
			b |= 0x20
		}
		if uint16(b) != c {
			return false
		}
	}
	return true
}

// indexUnits looks for the key unit with bytes.IndexByte on one of its
// bytes, the one that is not 0, over the bytes of u, and checks the other
// and the rest of the literal where it finds the unit.
func (l *reLiteral) indexUnits(u []uint16, from int) int {
	k, kc, kf := l.key, l.units[l.key], l.fold[l.key]
	last := len(u) - len(l.units) + k
	if !kf && from+k <= last {
		key := [1]uint16{kc}
		pair := unitBytes(key[:])
		half := 0 // which byte of a unit is looked for
		if pair[0] == 0 {
			half = 1
		}
		want, other := pair[half], pair[1-half]
		b := unitBytes(u[:last+1])
		for off := 2 * (from + k); ; {
			j := bytes.IndexByte(b[off:], want)
			if j < 0 {
				return -1
			}
			j += off
			off = j + 1
			if j&1 != half {
				continue // the other byte of a unit
			}
			if i := j / 2; b[j^1] == other && l.equalUnits(u[i-k:]) {
				return i - k
			}
		}
	}
	for i := from + k; i <= last; i++ {
		c := u[i]
		if kf && c < 0x80 {
			c |= 0x20
		}
		if c != kc {
			continue
		}
		if l.equalUnits(u[i-k:]) {
			return i - k
		}
	}
	return -1
}

// match returns the match of a whole literal at or after pos (exactly at
// pos when sticky), in sub's positions, or nil.
func (l *reLiteral) match(sub *reSubject, pos int, sticky bool) []int {
	at := l.find(sub, sub.toUnit(pos), sticky)
	if at < 0 {
		return nil
	}
	return []int{sub.toByte(at), sub.toByte(at + len(l.units))}
}

// find returns the code unit index of the literal at or after u (exactly
// at u when sticky), or -1.
func (l *reLiteral) find(sub *reSubject, u int, sticky bool) int {
	n := sub.s.Len()
	if !sticky {
		return l.index(sub, u, n)
	}
	if u+len(l.units) <= n && l.index(sub, u, u+len(l.units)) == u {
		return u
	}
	return -1
}

// unitRank ranks a code unit by how rarely it occurs in text, markup and
// code: the scan looks first for the literal's rarest unit, as most of the
// subject's units are then skipped by bytes.IndexByte. Units past ASCII
// rank above every ASCII one; the order of the ASCII ones is rough.
func unitRank(c uint16) int {
	if c >= 0x80 {
		return len(unitsByFrequency) + 1
	}
	if i := strings.IndexByte(unitsByFrequency, byte(c)); i >= 0 {
		return i
	}
	return len(unitsByFrequency)
}

// unitsByFrequency lists ASCII from the most frequent; control characters
// and anything not listed are rarer than all of these.
const unitsByFrequency = " etaoinsrhldcu\"m=/.,<>pfg-wy:b_\\v0k1'2;()\n\t&%#?x3456789jqzETAOINSRHLDCUMPFGWYBVKXJQZ[]{}+*!@$^`|~\r"

func (l *reLiteral) equalUnits(u []uint16) bool {
	for i, c := range l.units {
		b := u[i]
		if l.fold[i] && b < 0x80 {
			b |= 0x20
		}
		if b != c {
			return false
		}
	}
	return true
}

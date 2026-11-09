package engine

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Reads of a rope that need one pass over some of its units read them in
// its leaves instead of a flattened copy: a search (IndexOf), a comparison
// at an offset (hasSubstringAt), the conversion to a Go string. Flattening
// copies the whole rope, 2n bytes for UTF-16 contents, to read them once;
// the rendered page an SSR bundle searches with includes and the host then
// converts was copied twice that way.

// ropeWalkDepth bounds the stack of a ropeWalk, the right children of the
// nodes it went left at, and the nodes seek descends through. A rope
// deeper than that, a chain of `s += x` on a rope (left-deep) or of
// `s = x + s` (right-deep), is flattened instead.
const ropeWalkDepth = 48

// ropeWalk visits the leaves of a rope in order without flattening it. A
// leaf is a flat child, or the contents a flattening of a node published,
// which stand for all of the node's leaves. The rope's shape never changes
// but for the children a flattening releases after it published (flatten),
// so a walk that runs while another goroutine flattens sees the same units.
type ropeWalk struct {
	next  *String // the node leaf descends from, nil to pop the stack
	sp    int
	stack [ropeWalkDepth]*String
}

// leaf returns the next leaf, nil after the last one, in *v when it is a
// node's published contents, as flat does; ok is false when the rope is too
// deep on the left to walk.
func (w *ropeWalk) leaf(v *String) (f *String, ok bool) {
	s := w.next
	w.next = nil
	if s == nil {
		if w.sp == 0 {
			return nil, true
		}
		w.sp--
		s = w.stack[w.sp]
	}
	for s.kind == strRope {
		l, r := s.children()
		if l == nil {
			*v, _ = s.published()
			return v, true
		}
		if w.sp == len(w.stack) {
			return nil, false
		}
		w.stack[w.sp] = r
		w.sp++
		s = l
	}
	return s, true
}

// seek starts the walk of the rope s at the leaf that holds unit i and
// returns the offset of that leaf in s; ok is false when the leaf is more
// than ropeWalkDepth nodes down.
func (w *ropeWalk) seek(s *String, i int) (off int, ok bool) {
	for d := 0; s.kind == strRope; d++ {
		l, r := s.children()
		if l == nil {
			break
		}
		if d == ropeWalkDepth {
			return 0, false
		}
		if i-off < int(l.n) {
			w.stack[w.sp] = r
			w.sp++
			s = l
		} else {
			off += int(l.n)
			s = r
		}
	}
	w.next = s
	return off, true
}

// ropeSearchMax is the longest search string ropeIndexOf takes, and
// ropeSearchLeaves the most leaves it reads: past them the search flattens
// the rope, whose copy then serves the searches that follow, as a loop of
// indexOf calls on one rope would otherwise read its leaves again each time.
const (
	ropeSearchMax    = 64
	ropeSearchLeaves = 16
)

// ropeIndexOf is IndexOf of the rope s, from+sub.Len() <= s.Len(), searching
// its leaves from the one that holds unit from: an occurrence inside a leaf
// is a flat search of the leaf, and one across leaves is found in the
// window of units before a leaf, at most m-1 of them, followed by its first
// m-1. ok is false when the search is not done that way: sub is longer than
// ropeSearchMax, or the rope too deep or of too many leaves.
//
//go:noinline
func ropeIndexOf(s, sub *String, from int) (int, bool) {
	m := int(sub.n)
	if m > ropeSearchMax {
		return 0, false
	}
	fsub := sub
	if sub.needsFlat() {
		fsub = sub.flat(new(String))
	}
	var w ropeWalk
	off, ok := w.seek(s, from)
	if !ok {
		return 0, false
	}
	var v String
	// win[:nw] holds the units of s before off at which an occurrence that
	// ends past off may start: at most m-1, none before from.
	var win [2 * ropeSearchMax]uint16
	nw := 0
	for leaves := 0; ; leaves++ {
		f, ok := w.leaf(&v)
		if !ok {
			return 0, false
		}
		if f == nil {
			return -1, true
		}
		if leaves == ropeSearchLeaves {
			return 0, false
		}
		n := int(f.n)
		k := min(m-1, n)
		for i := range k {
			win[nw+i] = f.at(i)
		}
		// An occurrence that starts in the window ends in the first k units
		// of f, or past f when f is shorter than m-1, and is then in the
		// window of the next leaf; one inside f starts later.
		if i := windowIndex(win[:nw+k], nw, fsub); i >= 0 {
			return off - nw + i, true
		}
		if i := flatIndexOf(f, fsub, max(from-off, 0)); i >= 0 {
			return off + i, true
		}
		keep := off + n - max(off+n-(m-1), from)
		if n >= keep {
			for i := range keep {
				win[i] = f.at(n - keep + i)
			}
		} else {
			copy(win[:keep], win[nw+n-keep:nw+n])
		}
		nw = keep
		off += n
	}
}

// windowIndex returns the first q < nw at which sub occurs in u, or -1.
func windowIndex(u []uint16, nw int, sub *String) int {
	m := int(sub.n)
	if nw == 0 {
		return -1
	}
	first := sub.at(0)
outer:
	for q := 0; q < nw && q+m <= len(u); q++ {
		if u[q] != first {
			continue
		}
		for j := 1; j < m; j++ {
			if u[q+j] != sub.at(j) {
				continue outer
			}
		}
		return q
	}
	return -1
}

// ropeHasAt is hasSubstringAt of the rope s and the flat sub, at+sub.Len()
// <= s.Len(), comparing sub with the leaves that hold units at to
// at+sub.Len()-1; ok is false when they are too deep to reach.
func ropeHasAt(s, sub *String, at int) (match, ok bool) {
	var w ropeWalk
	off, ok := w.seek(s, at)
	if !ok {
		return false, false
	}
	var v String
	for k, m := 0, int(sub.n); k < m; {
		f, ok := w.leaf(&v)
		if !ok {
			return false, false
		}
		// f holds units off to off+f.n-1 of s, at+k among them.
		i := at + k - off
		n := min(m-k, int(f.n)-i)
		if f.kind == strASCII && sub.kind == strASCII {
			if f.s[i:i+n] != sub.s[k:k+n] {
				return false, true
			}
		} else {
			for j := range n {
				if f.at(i+j) != sub.at(k+j) {
					return false, true
				}
			}
		}
		k += n
		off += int(f.n)
	}
	return true, true
}

// flatIndexOf is IndexOf of flat strings, from >= 0.
func flatIndexOf(f, sub *String, from int) int {
	if from > int(f.n)-int(sub.n) {
		return -1
	}
	if f.kind == strASCII && sub.kind == strASCII {
		i := strings.Index(f.s[from:], sub.s)
		if i < 0 {
			return -1
		}
		return from + i
	}
	if f.kind == strASCII && sub.kind == strUTF16 {
		return -1
	}
	// A direct scan: each unit equal to the first of sub, then the rest of
	// sub against the slice of the text at that position, whose bounds are
	// checked once. hits counts the units the rest matched before a
	// mismatch; when they outweigh the positions scanned, the linear search
	// takes over (directScanSwitch, string_search.go).
	m := int(sub.n)
	first := sub.at(0)
	u := f.units()
	last := len(u) - m
	hits := 0
outer:
	for i := from; i <= last; i++ {
		if u[i] != first {
			continue
		}
		t := u[i : i+m]
		for j := 1; j < len(t); j++ {
			if t[j] != sub.at(j) {
				hits += j
				if directScanSwitch(m, hits, i-from, last-i) {
					return searchFlatUnits(f, sub, i+1, false)
				}
				continue outer
			}
		}
		return i
	}
	return -1
}

// ropeGoString is GoString of the rope s with a UTF-16 leaf, encoding its
// leaves to UTF-8 without the flat copy of its units: one pass measures the
// result, the next writes it. ok is false when every leaf is ASCII, whose
// flattened bytes are the Go string itself, or the rope is too deep.
//
//go:noinline
func ropeGoString(s *String) (string, bool) {
	var w ropeWalk
	w.next = s
	var v String
	n, hi, wide := 0, uint16(0), false
	for {
		f, ok := w.leaf(&v)
		if !ok {
			return "", false
		}
		if f == nil {
			break
		}
		if f.kind == strASCII {
			n += len(f.s) + loneLen(hi)
			hi = 0
			continue
		}
		wide = true
		var k int
		k, hi = utf8Len(f.units(), hi)
		n += k
	}
	if !wide {
		return "", false
	}
	n += loneLen(hi)
	b := make([]byte, 0, n)
	w = ropeWalk{next: s}
	hi = 0
	for {
		f, _ := w.leaf(&v) // as deep as the first pass found it
		if f == nil {
			break
		}
		if f.kind == strASCII {
			if hi != 0 {
				b, hi = utf8.AppendRune(b, utf8.RuneError), 0
			}
			b = append(b, f.s...)
			continue
		}
		b, hi = appendUTF8(b, f.units(), hi)
	}
	if hi != 0 {
		b = utf8.AppendRune(b, utf8.RuneError)
	}
	return bytesString(b), true
}

// loneLen is the length of what a pending high surrogate hi (0 for none)
// is written as when no low surrogate follows: U+FFFD, 3 bytes.
func loneLen(hi uint16) int {
	if hi != 0 {
		return 3
	}
	return 0
}

func isHighUnit(c uint16) bool { return c >= 0xD800 && c < 0xDC00 }
func isLowUnit(c uint16) bool  { return c >= 0xDC00 && c < 0xE000 }

// utf8Len is the length of the UTF-8 form GoString writes of the units u
// (lone surrogates as U+FFFD) after hi, a high surrogate the units before u
// ended with, or 0. A high surrogate that ends u is not counted: it is
// returned, to pair with the first unit of what follows.
func utf8Len(u []uint16, hi uint16) (n int, end uint16) {
	i := 0
	if hi != 0 {
		if len(u) > 0 && isLowUnit(u[0]) {
			n, i = 4, 1
		} else {
			n = 3
		}
	}
	for ; i < len(u); i++ {
		switch c := u[i]; {
		case c < 0x80:
			n++
		case c < 0x800:
			n += 2
		case isHighUnit(c):
			if i+1 == len(u) {
				return n, c
			}
			if isLowUnit(u[i+1]) {
				n += 4
				i++
			} else {
				n += 3
			}
		default: // a lone low surrogate is U+FFFD, 3 bytes too
			n += 3
		}
	}
	return n, 0
}

// appendUTF8 appends the UTF-8 form of the units u to b as utf8Len measures
// it, hi and end alike.
func appendUTF8(b []byte, u []uint16, hi uint16) ([]byte, uint16) {
	i := 0
	if hi != 0 {
		if len(u) > 0 && isLowUnit(u[0]) {
			b, i = utf8.AppendRune(b, utf16.DecodeRune(rune(hi), rune(u[0]))), 1
		} else {
			b = utf8.AppendRune(b, utf8.RuneError)
		}
	}
	for ; i < len(u); i++ {
		c := u[i]
		if c < 0x80 {
			b = append(b, byte(c))
			continue
		}
		if isHighUnit(c) {
			if i+1 == len(u) {
				return b, c
			}
			if isLowUnit(u[i+1]) {
				b = utf8.AppendRune(b, utf16.DecodeRune(rune(c), rune(u[i+1])))
				i++
				continue
			}
		}
		// A lone surrogate is not a valid rune: AppendRune writes U+FFFD.
		b = utf8.AppendRune(b, rune(c))
	}
	return b, 0
}

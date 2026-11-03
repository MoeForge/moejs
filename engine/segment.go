package engine

import (
	"math/bits"
	"sync"
)

//go:generate go run ../internal/gen/ucd -seg unicode_seg_tables.go -segtest testdata

// Text segmentation for Intl.Segmenter (ECMA-402 §18): the extended
// grapheme clusters, words and sentences of UAX #29 over the tables in
// unicode_seg_tables.go, generated from the same pinned UCD version as the
// regexp and normalization tables. Each granularity's table is decoded on
// first use and shared by the process; realms carry nothing.
//
// Text is UTF-16 code units. A lone surrogate is a code point of its own
// with the properties of an unassigned one (Other everywhere, as in ICU),
// and a boundary never falls between the units of a surrogate pair.
//
// segNext finds the end of the segment that starts at a boundary by running
// the rules forward from it: no rule looks back across a boundary, so the
// scan needs no state from before it. segStart backs up to a boundary that
// holds whatever precedes it (two code points that no rule can keep
// together, the end of a paragraph or of a sentence) and scans forward from
// there.
//
// Words have no dictionary: Han, kana and the Complex_Context scripts (Thai,
// Lao, Khmer, Myanmar, ...) get the UAX #29 boundaries, one segment per
// ideograph, where ICU looks words up; and Hangul syllables are letters as
// UAX #29 says, where ICU keeps them apart from other letters. isWordLike is
// what ICU's rule status would say of the segment (see segWordNext).

// segGranularity is an Intl.Segmenter granularity.
type segGranularity uint8

const (
	segGrapheme segGranularity = iota
	segWord
	segSentence
)

// segText is the text of a Segments object: the code units of a flat
// string, ascii when it is ASCII (u is nil then), u otherwise.
type segText struct {
	ascii string
	u     []uint16
}

func (t *segText) len() int {
	if t.u != nil {
		return len(t.u)
	}
	return len(t.ascii)
}

// at returns the code unit at index i (0 <= i < t.len()).
func (t *segText) at(i int) uint16 {
	if t.u != nil {
		return t.u[i]
	}
	return uint16(t.ascii[i])
}

// unit returns the code unit at index i as a code point, or -1 for a
// surrogate: the fast path of next, small enough to inline.
func (t *segText) unit(i int) rune {
	if t.u == nil {
		return rune(t.ascii[i])
	}
	if c := rune(t.u[i]); c&0xF800 != 0xD800 {
		return c
	}
	return -1
}

// next returns the code point at index i and the index after it.
func (t *segText) next(i int) (rune, int) {
	if c := t.unit(i); c >= 0 {
		return c, i + 1
	}
	return segSurrogate(t.u, i)
}

// before returns the code point that ends at index i and its start, not
// reading below lo (lo < i).
func (t *segText) before(lo, i int) (rune, int) {
	if t.u == nil {
		return rune(t.ascii[i-1]), i - 1
	}
	return segDecodeBefore(t.u, lo, i)
}

// segSurrogate returns the code point at u[i], a surrogate, and the index
// after it: a lone surrogate is a code point of its own.
func segSurrogate(u []uint16, i int) (rune, int) {
	c := rune(u[i])
	if c < 0xDC00 && i+1 < len(u) && u[i+1]&0xFC00 == 0xDC00 {
		return c<<10 + rune(u[i+1]) - (0xD800<<10 + 0xDC00 - 0x10000), i + 2
	}
	return c, i + 1
}

// segDecodeBefore returns the code point that ends at u[i] and its start,
// not reading below lo (lo < i).
func segDecodeBefore(u []uint16, lo, i int) (rune, int) {
	c := rune(u[i-1])
	if c&0xFC00 == 0xDC00 && i-2 >= lo && u[i-2]&0xFC00 == 0xD800 {
		return rune(u[i-2])<<10 + c - (0xD800<<10 + 0xDC00 - 0x10000), i - 2
	}
	return c, i - 1
}

// segNext returns the end of the segment that starts at the boundary i
// (0 <= i < t.len()), and for segWord whether the segment is word-like.
func segNext(g segGranularity, t *segText, i int) (end int, wordLike bool) {
	switch g {
	case segGrapheme:
		return segGraphemeNext(t, i), false
	case segWord:
		return segWordNext(t, i)
	}
	return segSentenceNext(t, i), false
}

// segStart returns the start of the segment that contains index i
// (0 <= i < t.len()): the last boundary <= i.
func segStart(g segGranularity, t *segText, i int) int {
	if g == segGrapheme && t.u == nil {
		if i > 0 && t.ascii[i] == '\n' && t.ascii[i-1] == '\r' {
			return i - 1
		}
		return i
	}
	p := i
	if t.u != nil && p > 0 && t.u[p]&0xFC00 == 0xDC00 && t.u[p-1]&0xFC00 == 0xD800 {
		p--
	}
	start := p
	for p > 0 && !segSure(g, t, p) {
		_, p = t.before(0, p)
	}
	if p == start {
		return p
	}
	for {
		e, _ := segNext(g, t, p)
		if e > i {
			return p
		}
		p = e
	}
}

// segSure reports whether there is a boundary at p (0 < p < t.len()) that
// holds whatever precedes it.
func segSure(g segGranularity, t *segText, p int) bool {
	switch g {
	case segGrapheme:
		gt := segGraphemes()
		c, _ := t.before(0, p)
		d, _ := t.next(p)
		return gt.pairs[gt.of(c)&15][gt.of(d)&15] == segBreak
	case segWord:
		return segWordSure(t, p)
	}
	return segSentenceSure(t, p)
}

// segTrie is a decoded segmentation table: one byte per code point, kept as
// data blocks of 64 code points, each stored once (ASCII is data[:128]).
type segTrie struct {
	bmp   [0x10000 >> 6]uint16 // offset in data of each block below U+10000
	sup   [256]uint16          // offset in index of each 4096 code points above
	index []uint16             // offsets in data of their blocks
	data  []uint8
}

func (t *segTrie) of(c rune) uint8 {
	u := uint32(c)
	if u < 0x80 {
		return t.data[u]
	}
	if u < 0x10000 {
		return t.data[int(t.bmp[u>>6])|int(u&63)]
	}
	return t.data[int(t.index[int(t.sup[(u>>12-16)&255])|int(u>>6&63)])|int(u&63)]
}

// decode decodes s, a table in the encoding of internal/gen/ucd -seg (see
// segTrie in internal/gen/ucd/seg.go).
func (t *segTrie) decode(s string) {
	np, i := uvarintAt(s, 0)
	palette := s[i : i+int(np)]
	i += int(np)
	width := bits.Len(uint(np - 1))
	nd, i := uvarintAt(s, i)
	ni, i := uvarintAt(s, i)
	t.data = make([]uint8, nd<<6)
	t.index = make([]uint16, ni<<6)
	for o := 0; o < len(t.data); {
		var r uint64
		r, i = uvarintAt(s, i)
		run := t.data[o : o+int(r>>width)+1]
		if v := palette[r&(1<<width-1)]; v != 0 {
			for k := range run {
				run[k] = v
			}
		}
		o += len(run)
	}
	fresh := 0
	i = segBlocks(t.bmp[:], s, i, &fresh)
	i = segBlocks(t.index, s, i, &fresh)
	fresh = 0
	segBlocks(t.sup[:], s, i, &fresh)
}

// segBlocks decodes the block numbers at s[i] into dst as offsets (64 times
// the number) and returns the index after them; *fresh is the number of the
// next block that appears for the first time.
func segBlocks(dst []uint16, s string, i int, fresh *int) int {
	for o := 0; o < len(dst); {
		var r, k uint64
		r, i = uvarintAt(s, i)
		run := dst[o : o+int(r>>1)]
		o += len(run)
		if r&1 == 0 {
			for j := range run {
				run[j] = uint16(*fresh << 6)
				*fresh++
			}
			continue
		}
		k, i = uvarintAt(s, i)
		for j := range run {
			run[j] = uint16(k << 6)
		}
	}
	return i
}

// Pair actions: what the rules say about a boundary between two classes.
const (
	segBreak  uint8 = iota // break
	segJoin                // no break
	segGB9c                // grapheme: no break if GB9c holds
	segGB11                // grapheme: no break if GB11 holds
	segRI                  // no break after an odd number of regional indicators (GB12, GB13, WB15, WB16)
	segAbsorb              // word: no break, and the class on the left stays (WB4)
	segSpace               // word: no break unless Extend, Format or ZWJ came between (WB3d)
	segAhead               // word: no break if the next base completes WB6, WB7b or WB12
	segBehind              // word: no break if the base before completes WB7, WB7c or WB11
)

// segGraphemeTables is the decoded grapheme table and its pair actions by
// the classes of two adjacent code points.
type segGraphemeTables struct {
	segTrie
	pairs [16][16]uint8
}

var (
	segGraphemeOnce sync.Once
	segGraphemeTab  segGraphemeTables
)

func segGraphemes() *segGraphemeTables {
	segGraphemeOnce.Do(segGraphemeTab.decode)
	return &segGraphemeTab
}

func (t *segGraphemeTables) decode() {
	t.segTrie.decode(segGraphemeTrie)
	for x := range uint8(16) {
		for y := range uint8(16) {
			t.pairs[x][y] = segGraphemePair(x, y)
		}
	}
}

// segGraphemePair returns the action of the grapheme cluster rules (UAX #29
// §3.1.1) between a code point of class x and one of class y.
func segGraphemePair(x, y uint8) uint8 {
	control := func(c uint8) bool { return c == segGControl || c == segGCR || c == segGLF }
	switch {
	case x == segGCR && y == segGLF: // GB3
		return segJoin
	case control(x) || control(y): // GB4, GB5
		return segBreak
	case x == segGL && (y == segGL || y == segGV || y == segGLV || y == segGLVT), // GB6
		(x == segGLV || x == segGV) && (y == segGV || y == segGT), // GB7
		(x == segGLVT || x == segGT) && y == segGT,                // GB8
		y == segGExtend || y == segGZWJ,                           // GB9
		y == segGSpacingMark,                                      // GB9a
		x == segGPrepend:                                          // GB9b
		return segJoin
	case (x == segGExtend || x == segGZWJ) && y == segGConsonant:
		return segGB9c
	case x == segGZWJ && y == segGExtPict:
		return segGB11
	case x == segGRI && y == segGRI:
		return segRI
	}
	return segBreak // GB999
}

// segGraphemeNext returns the end of the grapheme cluster that starts at i.
func segGraphemeNext(t *segText, i int) int {
	u := t.u
	if u == nil {
		// Every ASCII character is a cluster of its own but CR LF.
		if t.ascii[i] == '\r' && i+1 < len(t.ascii) && t.ascii[i+1] == '\n' {
			return i + 2
		}
		return i + 1
	}
	g := segGraphemes()
	c, j := rune(u[i]), i+1
	if c&0xF800 == 0xD800 {
		c, j = segSurrogate(u, i)
	}
	x := g.of(c) & 15
	for j < len(u) {
		c, k := rune(u[j]), j+1
		switch {
		case c < 0x80:
			// Nothing keeps an ASCII character with what precedes it but
			// CR LF (GB3) and Prepend before a non-control (GB9b).
			if x != segGPrepend && (c != '\n' || x != segGCR) {
				return j
			}
		case c&0xF800 == 0xD800:
			c, k = segSurrogate(u, j)
		}
		v := g.of(c)
		if v|x == 0 { // Other after Other (GB999)
			return j
		}
		y := v & 15
		switch g.pairs[x][y] {
		case segBreak:
			return j
		case segJoin:
		case segGB9c:
			if !g.gb9c(u, i, j) {
				return j
			}
		case segGB11:
			if !g.gb11(u, i, j) {
				return j
			}
		case segRI:
			if !g.oddRI(u, i, j) {
				return j
			}
		}
		x, j = y, k
	}
	return j
}

// gb9c reports whether GB9c keeps the consonant at u[j] with the cluster
// that starts at i: Consonant [Extend Linker]* Linker [Extend Linker]*
// comes before it.
func (g *segGraphemeTables) gb9c(u []uint16, i, j int) bool {
	linker := false
	for j > i {
		var c rune
		c, j = segDecodeBefore(u, i, j)
		p := g.of(c)
		switch {
		case p&segGLinker != 0:
			linker = true
		case p&segGInCBExtend == 0:
			return linker && p&15 == segGConsonant
		}
	}
	return false
}

// gb11 reports whether GB11 keeps the pictograph at u[j] with the cluster
// that starts at i: ExtPict Extend* ZWJ comes before it.
func (g *segGraphemeTables) gb11(u []uint16, i, j int) bool {
	j-- // the ZWJ
	for j > i {
		var c rune
		c, j = segDecodeBefore(u, i, j)
		if p := g.of(c) & 15; p != segGExtend {
			return p == segGExtPict
		}
	}
	return false
}

// oddRI reports whether an odd number of regional indicators come right
// before u[j] in the cluster that starts at i.
func (g *segGraphemeTables) oddRI(u []uint16, i, j int) bool {
	odd := false
	for j > i {
		var c rune
		c, j = segDecodeBefore(u, i, j)
		if g.of(c)&15 != segGRI {
			break
		}
		odd = !odd
	}
	return odd
}

// segWordTables is the decoded word table and its pair actions by the
// classes of the base before (WB4) and the code point after a position.
type segWordTables struct {
	segTrie
	pairs [32][32]uint8
}

var (
	segWordOnce sync.Once
	segWordTab  segWordTables
)

func segWords() *segWordTables {
	segWordOnce.Do(segWordTab.decode)
	return &segWordTab
}

func (t *segWordTables) decode() {
	t.segTrie.decode(segWordTrie)
	for x := range segWWSegSpace + 1 {
		for y := range segWWSegSpace + 1 {
			t.pairs[x][y] = segWordPair(x, y)
		}
	}
}

func segAHLetter(c uint8) bool { return c == segWALetter || c == segWHebrewLetter }

func segExFm(c uint8) bool { return c-segWExtend < 3 } // Extend, Format, ZWJ

// segWordPair returns the action of the word rules (UAX #29 §4.1.1) between
// a base of class x and a code point of class y. WB1-WB3c are handled by
// segWordNext.
func segWordPair(x, y uint8) uint8 {
	midNumLetQ := func(c uint8) bool { return c == segWMidNumLet || c == segWSingleQuote }
	switch {
	case y == segWCR || y == segWLF || y == segWNewline: // WB3b
		return segBreak
	case x == segWWSegSpace && y == segWWSegSpace:
		return segSpace
	case segExFm(y):
		return segAbsorb
	case segAHLetter(x) && segAHLetter(y), // WB5
		x == segWHebrewLetter && y == segWSingleQuote,                          // WB7a
		x == segWNumeric && y == segWNumeric,                                   // WB8
		segAHLetter(x) && y == segWNumeric, x == segWNumeric && segAHLetter(y), // WB9, WB10
		x == segWKatakana && y == segWKatakana, // WB13
		(segAHLetter(x) || x == segWNumeric || x == segWKatakana || x == segWExtendNumLet) && y == segWExtendNumLet, // WB13a
		x == segWExtendNumLet && (segAHLetter(y) || y == segWNumeric || y == segWKatakana):                          // WB13b
		return segJoin
	case segAHLetter(x) && (y == segWMidLetter || midNumLetQ(y)), // WB6
		x == segWHebrewLetter && y == segWDoubleQuote,          // WB7b
		x == segWNumeric && (y == segWMidNum || midNumLetQ(y)): // WB12
		return segAhead
	case (x == segWMidLetter || midNumLetQ(x)) && segAHLetter(y), // WB7
		x == segWDoubleQuote && y == segWHebrewLetter,          // WB7c
		(x == segWMidNum || midNumLetQ(x)) && y == segWNumeric: // WB11
		return segBehind
	case x == segWRI && y == segWRI:
		return segRI
	}
	return segBreak // WB999
}

// segWordNext returns the end of the word segment that starts at i and
// whether it is word-like.
//
// isWordLike follows ICU's rule status (word.txt): a segment ending in a
// base with segWWordLike is word-like (letter, number, kana, ideo), and so
// is one whose last base was joined by WB7a or WB13a (ICU's tagged pair
// rules), unless Extend, Format or ZWJ follow it there or after a
// segWBare base. Everything else (spaces, punctuation, emoji, a lone
// ExtendNumLet) is not.
func segWordNext(t *segText, i int) (int, bool) {
	w := segWords()
	n := t.len()
	c, j := t.next(i)
	last := w.of(c) // the base before j (WB4), with its flags
	x := last & 31
	switch x {
	case segWCR: // WB3, WB3a
		if j < n && t.at(j) == '\n' {
			return j + 1, false
		}
		return j, false
	case segWLF, segWNewline:
		return j, false
	}
	prev := uint8(31)   // the class of the base before x in the segment
	trail := false      // Extend, Format or ZWJ came after x
	zwj := x == segWZWJ // the code point before j is a ZWJ
	for j < n {
		c, k := t.unit(j), j+1
		if c < 0 {
			c, k = segSurrogate(t.u, j)
		}
		q := w.of(c)
		y := q & 31
		join := false
		switch w.pairs[x][y] {
		case segJoin:
			join = true
		case segAbsorb:
			trail, zwj, j = true, y == segWZWJ, k
			continue
		case segSpace:
			join = !trail
		case segAhead:
			join = w.ahead(t, x, y, k)
		case segBehind:
			join = segWordBehind(prev, x, y)
		case segRI:
			join = prev != segWRI
		}
		if !join && !(zwj && q&segWExtPict != 0) { // WB3c
			break
		}
		prev, x, last, trail, zwj, j = x, y, q, false, false, k
	}
	// A last base that is ExtendNumLet or Single_Quote and was joined at all
	// was joined by WB13a or WB7a.
	tagged := (x == segWExtendNumLet || x == segWSingleQuote) && prev != 31
	like := last&segWWordLike != 0 && (!trail || last&segWBare == 0) || tagged && !trail
	return j, like
}

// ahead reports whether the base after the code point of class y that ends
// at k completes the rule (WB6, WB7b or WB12) that x and y start.
func (w *segWordTables) ahead(t *segText, x, y uint8, k int) bool {
	for k < t.len() {
		var c rune
		c, k = t.next(k)
		z := w.of(c) & 31
		switch {
		case segExFm(z):
		case y == segWDoubleQuote:
			return z == segWHebrewLetter
		case x == segWNumeric:
			return z == segWNumeric
		default:
			return segAHLetter(z)
		}
	}
	return false
}

// segWordBehind reports whether the base of class w before x completes the
// rule (WB7, WB7c or WB11) that x and y end.
func segWordBehind(w, x, y uint8) bool {
	switch {
	case y == segWNumeric:
		return w == segWNumeric
	case x == segWDoubleQuote:
		return w == segWHebrewLetter
	}
	return segAHLetter(w)
}

// segWordSure reports whether there is a word boundary at p that holds
// whatever precedes it: around a line break, or between two bases no rule
// joins in any context.
func segWordSure(t *segText, p int) bool {
	w := segWords()
	c, _ := t.before(0, p)
	d, _ := t.next(p)
	x, y := w.of(c)&31, w.of(d)&31
	switch {
	case x == segWCR:
		return y != segWLF
	case x == segWLF || x == segWNewline || y == segWCR || y == segWLF || y == segWNewline:
		return true
	case segExFm(x):
		return false
	}
	return w.pairs[x][y] == segBreak
}

var (
	segSentenceOnce sync.Once
	segSentenceTab  segTrie
)

func segSentences() *segTrie {
	segSentenceOnce.Do(func() { segSentenceTab.decode(segSentenceTrie) })
	return &segSentenceTab
}

func segParaSep(c uint8) bool { return c-segSCR < 3 } // CR, LF, Sep

// segSentenceNext returns the end of the sentence that starts at i (UAX #29
// §5.1.1). Breaks fall only after a paragraph separator (SB4) and after a
// terminator sequence SATerm Close* Sp* ParaSep? that nothing continues
// (SB11).
func segSentenceNext(t *segText, i int) int {
	s := segSentences()
	n := t.len()
	var term uint8      // the terminator of the open sequence: ATerm, STerm or 0
	closeSp := false    // Close or Sp came after the terminator
	sp := false         // Sp came after it
	upperLower := false // the ATerm came after Upper or Lower (SB7)
	x := segSOther
	for j := i; j < n; {
		if term == 0 {
			// Outside a terminator sequence only a paragraph separator or
			// a terminator does anything but set x: skip to the next one,
			// and give x what the code points skipped would have.
			p := segSentenceSkip(t, s, j)
			if p == n {
				return n
			}
			if p > j {
				x = segSentenceLast(t, s, i, j, p, x)
				j = p
			}
		}
		c, k := t.unit(j), j+1
		if c < 0 {
			c, k = segSurrogate(t.u, j)
		}
		y := s.of(c)
		if j > i && (y == segSExtend || y == segSFormat) { // SB5
			j = k
			continue
		}
		if term != 0 {
			switch {
			case y == segSSContinue || y == segSSTerm || y == segSATerm, // SB8a
				y == segSClose && !sp,      // SB9
				y == segSSp, segParaSep(y), // SB9, SB10
				term == segSATerm && !closeSp && y == segSNumeric,             // SB6
				term == segSATerm && !closeSp && upperLower && y == segSUpper, // SB7
				term == segSATerm && segSB8(t, s, j):                          // SB8
			default:
				return j // SB11
			}
		}
		switch {
		case y == segSCR: // SB3, SB4
			if k < n && t.at(k) == '\n' {
				return k + 1
			}
			return k
		case segParaSep(y):
			return k
		case y == segSATerm || y == segSSTerm:
			term, closeSp, sp = y, false, false
			upperLower = x == segSUpper || x == segSLower
		case term != 0 && y == segSClose && !sp:
			closeSp = true
		case term != 0 && y == segSSp:
			closeSp, sp = true, true
		default:
			term = 0
		}
		x, j = y, k
	}
	return n
}

// segSentenceStops are the sentence classes of segSentenceSkip, and
// segSentenceASCIIStops the ASCII characters of those classes, all below
// 64: LF, CR, '!', '.' and '?' (TestSegSentenceASCIIStops).
const (
	segSentenceStops             = 1<<segSCR | 1<<segSLF | 1<<segSSep | 1<<segSATerm | 1<<segSSTerm
	segSentenceASCIIStops uint64 = 1<<'\n' | 1<<'\r' | 1<<'!' | 1<<'.' | 1<<'?'
)

// segSentenceSkip returns the index of the first code point from j that is
// a paragraph separator or a terminator, or t.len().
func segSentenceSkip(t *segText, s *segTrie, j int) int {
	if t.u == nil {
		a := t.ascii
		for j < len(a) && (a[j] >= 64 || segSentenceASCIIStops>>a[j]&1 == 0) {
			j++
		}
		return j
	}
	u := t.u
	for j < len(u) {
		switch c := u[j]; {
		case c < 0x80:
			if c < 64 && segSentenceASCIIStops>>c&1 != 0 {
				return j
			}
			j++
		case c&0xF800 != 0xD800:
			// segTrie.of below U+10000.
			if segSentenceStops>>(s.data[int(s.bmp[c>>6])|int(c&63)]&15)&1 != 0 {
				return j
			}
			j++
		default:
			d, k := segSurrogate(u, j)
			if segSentenceStops>>(s.of(d)&15)&1 != 0 {
				return j
			}
			j = k
		}
	}
	return j
}

// segSentenceLast returns what segSentenceNext, starting at i, leaves in x
// after the code points [j, p), none of which ends a sentence or starts a
// terminator sequence, when it had x before them: the class of the last
// one that SB5 does not fold into the one before it, an Extend or Format
// only at i.
func segSentenceLast(t *segText, s *segTrie, i, j, p int, x uint8) uint8 {
	for p > j {
		var c rune
		c, p = t.before(j, p)
		if y := s.of(c); p == i || y != segSExtend && y != segSFormat {
			return y
		}
	}
	return x
}

// segSB8 reports whether the code points from j that are no OLetter, Upper,
// Lower, ParaSep or SATerm are followed by a Lower (SB8).
func segSB8(t *segText, s *segTrie, j int) bool {
	for j < t.len() {
		var c rune
		c, j = t.next(j)
		switch s.of(c) {
		case segSLower:
			return true
		case segSOLetter, segSUpper, segSCR, segSLF, segSSep, segSATerm, segSSTerm:
			return false
		}
	}
	return false
}

// segSentenceSure reports whether there is a sentence boundary at p that
// holds whatever precedes it: after a paragraph separator, or before a
// letter after STerm Close* Sp*, or after ATerm Close* Sp* when neither SB7
// nor SB8 can join them.
func segSentenceSure(t *segText, p int) bool {
	s := segSentences()
	c, k := t.before(0, p)
	switch s.of(c) {
	case segSLF, segSSep:
		return true
	case segSCR:
		return t.at(p) != '\n'
	}
	d, _ := t.next(p)
	y := s.of(d)
	if y != segSUpper && y != segSLower && y != segSOLetter {
		return false
	}
	closeSp, sawClose := false, false
	for {
		switch s.of(c) {
		case segSExtend, segSFormat:
		case segSSp:
			if sawClose {
				return false
			}
			closeSp = true
		case segSClose:
			closeSp, sawClose = true, true
		case segSSTerm:
			return true
		case segSATerm:
			switch {
			case y == segSLower:
				return false
			case y == segSOLetter || closeSp:
				return true
			}
			// Upper right after the ATerm: SB7 joins them after Upper or
			// Lower.
			for k > 0 {
				c, k = t.before(0, k)
				if b := s.of(c); b != segSExtend && b != segSFormat {
					return b != segSUpper && b != segSLower
				}
			}
			return true
		default:
			return false
		}
		if k == 0 {
			return false
		}
		c, k = t.before(0, k)
	}
}

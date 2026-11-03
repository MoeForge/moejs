package node

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Glob patterns, as Node's fs.glob reads them through minimatch: "/"
// separates segments; "**" as a whole segment matches any number of
// directories; in a segment "*" matches any run of characters, "?" one,
// "[...]" one of a class ("[!...]" or "[^...]" one not in it, with ranges
// and POSIX classes such as [:alpha:]), "\" escapes the next character,
// and the extglobs "@(a|b)", "?(a|b)", "*(a|b)", "+(a|b)" and "!(a|b)"
// match one, at most one, any number, at least one of the alternatives,
// or anything but them. Braces expand first: "{a,b}" into a and b,
// "{1..3}" into 1, 2 and 3, nested ones too. A leading "." of a name is
// matched only by a "." of the pattern (dot: false), and "." and ".."
// never by a wildcard.

// globPattern is a pattern after brace expansion, split into segments.
type globPattern struct {
	segs []globSeg
	// prefix is the leading run of literal segments, joined: the
	// directory the walk starts at, as the pattern names it ("" for
	// none, "/" for an absolute pattern's root).
	prefix string
	// dirOnly is set for a pattern that ends in "/": it matches
	// directories only.
	dirOnly bool
}

// globSeg is a segment: a literal name, "**", or a matcher.
type globSeg struct {
	lit      string
	globstar bool
	m        *segMatcher // nil for a literal
}

// Limits on a pattern, so that what compiling and matching it take stays
// bounded: minimatch's 64 KiB on the pattern, a bound on what its braces
// expand to, and one on the size of a segment's compiled expression.
const (
	maxPatternLength  = 64 << 10
	maxExpandedLength = 1 << 20
	maxRegexpInsts    = 1 << 16
	// maxCallInsts bounds the instructions of all the expressions one
	// call compiles, and instBytes is what one costs the memory limit.
	maxCallInsts = 1 << 17
	instBytes    = 64
	// maxMatchWork bounds the work of one call's matches: the bytes of
	// the names times the instructions of the expressions run over them.
	maxMatchWork = 1 << 31
	// checkWork is the work between two checks of the interrupt and Close.
	checkWork = 1 << 22
)

// globCompile is what compiling the patterns of one call took so far: it
// bounds the instructions, charges their memory and checks the runtime's
// interrupt between expressions.
type globCompile struct {
	insts    int
	toks     int
	expanded int // the bytes the braces of the call's patterns expanded to
	charge   func(n int) error
	halt     func() error
}

// add counts an expression of n instructions.
func (cc *globCompile) add(n int) error {
	if cc == nil {
		return nil
	}
	if cc.insts += n; cc.insts > maxCallInsts {
		return errTooComplex
	}
	if err := cc.chargeBytes(n * instBytes); err != nil {
		return err
	}
	return cc.check()
}

// addToks counts n tokens parsed.
func (cc *globCompile) addToks(n int) error {
	if cc == nil {
		return nil
	}
	if cc.toks += n; cc.toks > maxCallToks {
		return errTooComplex
	}
	return cc.chargeBytes(n * tokBytes)
}

// chargeBytes charges n bytes of what compiling builds.
func (cc *globCompile) chargeBytes(n int) error {
	if cc == nil || cc.charge == nil {
		return nil
	}
	return cc.charge(n)
}

// check checks the runtime's interrupt.
func (cc *globCompile) check() error {
	if cc == nil || cc.halt == nil {
		return nil
	}
	return cc.halt()
}

// matchMeter counts the work of one call's matches, checks the runtime's
// interrupt and Close every checkWork of it, and fails the call past
// maxMatchWork.
type matchMeter struct {
	work, checked int
	halt          func() error
}

var errMatchTooComplex = &globError{"The glob pattern is too complex to match: its expressions take too long over these names"}

func (m *matchMeter) add(units int) error {
	if m.work += units; m.work > maxMatchWork {
		return errMatchTooComplex
	}
	if m.work-m.checked >= checkWork {
		m.checked = m.work
		if m.halt != nil {
			return m.halt()
		}
	}
	return nil
}

// globError is the TypeError of a pattern too long or complex to match
// safely.
type globError struct{ msg string }

func (e *globError) Error() string { return e.msg }

var errPatternTooLong = &globError{"pattern is too long"}

// compileGlob compiles the pattern p into the patterns its braces expand
// to.
func compileGlob(p string, cc *globCompile) ([]globPattern, error) {
	if len(p) > maxPatternLength {
		return nil, errPatternTooLong
	}
	// Node.js reads a backslash as a separator, never as an escape
	// (minimatch's windowsPathsNoEscape), on every system.
	p = strings.ReplaceAll(p, `\`, "/")
	expanded, err := expandBraces(p, cc)
	if err != nil {
		return nil, err
	}
	var out []globPattern
	for _, x := range expanded {
		g, err := compileOne(x, cc)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func compileOne(p string, cc *globCompile) (globPattern, error) {
	var g globPattern
	abs := strings.HasPrefix(p, "/")
	parts := strings.Split(p, "/")
	if abs {
		parts = parts[1:]
	}
	if len(parts) > 0 && parts[len(parts)-1] == "" && len(parts) > 1 {
		g.dirOnly = true
		parts = parts[:len(parts)-1]
	}
	// Drop empty and "." segments, as minimatch does.
	var segs []string
	for i, s := range parts {
		if s == "" || (s == "." && (i > 0 || len(parts) > 1)) {
			continue
		}
		segs = append(segs, s)
	}
	var prefix []string
	literal := true
	for i, s := range segs {
		var seg globSeg
		switch {
		case s == "**":
			if n := len(g.segs); n > 0 && g.segs[n-1].globstar {
				// "**/**" is "**", as minimatch reads it.
				continue
			}
			seg.globstar = true
		case !hasMagic(s):
			seg.lit = unescape(s)
		default:
			m, err := compileSegment(s, cc)
			if err != nil {
				return g, err
			}
			seg.m = m
		}
		if literal && seg.m == nil && !seg.globstar && i < len(segs)-1 {
			prefix = append(prefix, seg.lit)
			continue
		}
		literal = false
		g.segs = append(g.segs, seg)
	}
	g.prefix = strings.Join(prefix, "/")
	if abs {
		g.prefix = "/" + g.prefix
	}
	return g, nil
}

// hasMagic reports whether the segment s has a character that matches
// more than itself.
func hasMagic(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '?', '[':
			return true
		case '\\':
			i++
		case '@', '+', '!':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		}
	}
	return false
}

// unescape drops the backslashes of a literal segment.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// --- braces -------------------------------------------------------------------------

// maxExpansions bounds what one pattern's braces expand to: the first
// maxExpansions patterns, in order, are kept. maxBraceDepth bounds how
// deep braces nest.
const (
	maxExpansions = 4096
	maxBraceDepth = 1024
)

var (
	errExpandsTooLong = &globError{"The braces of the glob patterns expand to more than " + itoa(maxExpandedLength) + " bytes of patterns"}
	errBracesTooDeep  = &globError{"The glob pattern nests braces more than " + itoa(maxBraceDepth) + " deep"}
)

// What the expansion charges the memory limit: per byte of the pattern
// for its scan, per node of its graph, and per pattern it expands to,
// beyond the pattern's bytes.
const (
	braceScanBytes = 9
	braceNodeBytes = 96
	expansionBytes = 16
)

// braceNode is a node of the graph a pattern's braces make: a literal,
// a list ("{a,b}") whose alternatives each begin a path that goes on to
// next (a nil one at once), or a sequence ("{1..3}"). An expansion is a
// path from the first node to nil, and the paths taken in order, the
// alternatives of each group in turn and the last group's first, are the
// expansions in order.
type braceNode struct {
	lit  string
	alts []*braceNode
	seq  *braceSeq
	next *braceNode
}

// choices returns the number of ways through the group n.
func (n *braceNode) choices() int {
	if n.seq != nil {
		return n.seq.count
	}
	return len(n.alts)
}

// braceSeq is a sequence's values: count of them from a, step apart, up or
// down; numbers padded with zeros to width, or characters.
type braceSeq struct {
	a     int64
	step  uint64
	down  bool
	count int
	width int
	chars bool
}

// appendValue appends the sequence's k-th value to buf.
func (q *braceSeq) appendValue(buf []byte, k int) []byte {
	// k*step is at most the distance from a to the end.
	v := uint64(q.a) + uint64(k)*q.step
	if q.down {
		v = uint64(q.a) - uint64(k)*q.step
	}
	if q.chars {
		return utf8.AppendRune(buf, rune(v))
	}
	var tmp [24]byte
	d := strconv.AppendInt(tmp[:0], int64(v), 10)
	pad := q.width - len(d)
	if pad <= 0 {
		return append(buf, d...)
	}
	if d[0] == '-' {
		// The zeros go after the sign, as brace-expansion puts them.
		buf = append(buf, '-')
		d = d[1:]
	}
	for ; pad > 0; pad-- {
		buf = append(buf, '0')
	}
	return append(buf, d...)
}

// braceScan is what one pass over a pattern finds of its braces.
type braceScan struct {
	close []int32 // at a "{": the index of the "}" that closes it, or -1
	comma []int32 // at a "{" and at each of its top-level ",": the next of these, or -1
	inner []bool  // at a "{": whether a brace opens in it
}

func scanBraces(p string) *braceScan {
	n := len(p)
	s := &braceScan{close: make([]int32, n), comma: make([]int32, n), inner: make([]bool, n)}
	var open, last []int32 // the braces open, and the last "{" or "," of each
	for i := 0; i < n; i++ {
		switch p[i] {
		case '{':
			if len(open) > 0 {
				s.inner[open[len(open)-1]] = true
			}
			s.close[i], s.comma[i] = -1, -1
			open = append(open, int32(i))
			last = append(last, int32(i))
		case ',':
			if k := len(last) - 1; k >= 0 {
				s.comma[last[k]], s.comma[i] = int32(i), -1
				last[k] = int32(i)
			}
		case '}':
			if k := len(open) - 1; k >= 0 {
				s.close[open[k]] = int32(i)
				open, last = open[:k], last[:k]
			}
		}
	}
	return s
}

// braceParser builds the graph of a pattern.
type braceParser struct {
	p     string
	s     *braceScan
	nodes int
	depth int
}

// path builds the nodes of p[lo:hi], whose paths go on to next, and
// returns the first. A "{" begins a group when a "}" closes it and it has
// a top-level "," or is a sequence; otherwise it is a character, and the
// braces in it are read as the rest are. The braces of p[lo:hi] close in
// it, as each alternative's do.
func (b *braceParser) path(lo, hi int, next *braceNode) (*braceNode, error) {
	type piece struct {
		lo, hi int // the literal, or the group from "{" to "}"
		group  bool
		seq    *braceSeq
	}
	var pieces []piece
	last := lo
	for i := lo; i < hi; i++ {
		if b.p[i] != '{' || b.s.close[i] < 0 {
			continue
		}
		c := int(b.s.close[i])
		var seq *braceSeq
		if b.s.comma[i] < 0 {
			// A sequence has no braces in it.
			if b.s.inner[i] {
				continue
			}
			if seq = parseSeq(b.p[i+1 : c]); seq == nil {
				continue
			}
		}
		pieces = append(pieces, piece{lo: last, hi: i}, piece{lo: i, hi: c, group: true, seq: seq})
		i, last = c, c+1
	}
	pieces = append(pieces, piece{lo: last, hi: hi})
	for k := len(pieces) - 1; k >= 0; k-- {
		pc := pieces[k]
		b.nodes++
		switch {
		case !pc.group:
			if pc.hi > pc.lo {
				next = &braceNode{lit: b.p[pc.lo:pc.hi], next: next}
			}
			continue
		case pc.seq != nil:
			next = &braceNode{seq: pc.seq, next: next}
			continue
		}
		if b.depth++; b.depth > maxBraceDepth {
			return nil, errBracesTooDeep
		}
		g := &braceNode{next: next}
		start := pc.lo + 1
		for j := int(b.s.comma[pc.lo]); ; j = int(b.s.comma[j]) {
			end := j
			if j < 0 {
				end = pc.hi
			}
			alt, err := b.path(start, end, next)
			if err != nil {
				return nil, err
			}
			g.alts = append(g.alts, alt)
			b.nodes++
			if j < 0 {
				break
			}
			start = j + 1
		}
		b.depth--
		next = g
	}
	return next, nil
}

// expandBraces expands the braces of p, as the brace-expansion package
// does for minimatch, into the first maxExpansions patterns. It walks the
// graph of p's braces, so that its work is the length of what it expands
// to and of p; it fails the moment what the call's patterns expanded to
// comes to more than maxExpandedLength bytes, checks the runtime's
// interrupt as it goes, and charges what it builds.
func expandBraces(p string, cc *globCompile) ([]string, error) {
	if strings.IndexByte(p, '{') < 0 {
		return []string{p}, nil
	}
	if err := cc.chargeBytes(braceScanBytes * len(p)); err != nil {
		return nil, err
	}
	b := &braceParser{p: p, s: scanBraces(p)}
	first, err := b.path(0, len(p), nil)
	if err != nil {
		return nil, err
	}
	b.s = nil
	if err := cc.chargeBytes(braceNodeBytes * b.nodes); err != nil {
		return nil, err
	}
	// A choice is a group on the path, the way taken through it, and the
	// length of the pattern before it.
	type choice struct {
		g       *braceNode
		k, mark int
	}
	var (
		stack []choice
		buf   []byte
		out   []string
		total int
		steps int
	)
	if cc != nil {
		total = cc.expanded
		defer func() { cc.expanded = total }()
	}
	step := func() error {
		if steps++; steps&4095 == 0 {
			return cc.check()
		}
		return nil
	}
	enter := func(g *braceNode, k int) *braceNode {
		if g.seq != nil {
			buf = g.seq.appendValue(buf, k)
			return g.next
		}
		return g.alts[k]
	}
	for n := first; ; {
		// Take the first way through each group to the end of a path.
		for n != nil {
			if err := step(); err != nil {
				return nil, err
			}
			if n.alts == nil && n.seq == nil {
				buf = append(buf, n.lit...)
				n = n.next
			} else {
				stack = append(stack, choice{g: n, mark: len(buf)})
				n = enter(n, 0)
			}
			if total+len(buf) > maxExpandedLength {
				return nil, errExpandsTooLong
			}
		}
		if total += len(buf); total > maxExpandedLength {
			return nil, errExpandsTooLong
		}
		if err := cc.chargeBytes(len(buf) + expansionBytes); err != nil {
			return nil, err
		}
		out = append(out, string(buf))
		if len(out) == maxExpansions {
			return out, nil
		}
		// Back to the last group with a way left.
		for {
			if len(stack) == 0 {
				return out, nil
			}
			if err := step(); err != nil {
				return nil, err
			}
			c := &stack[len(stack)-1]
			if c.k+1 < c.g.choices() {
				c.k++
				buf = buf[:c.mark]
				n = enter(c.g, c.k)
				break
			}
			stack = stack[:len(stack)-1]
		}
	}
}

// parseSeq parses a sequence's body, "1..3", "a..e" or "1..10..2", or
// returns nil.
func parseSeq(body string) *braceSeq {
	parts := strings.Split(body, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return nil
	}
	q := &braceSeq{step: 1}
	if len(parts) == 3 {
		s, err := strconv.Atoi(parts[2])
		if err != nil || s == 0 {
			return nil
		}
		if s < 0 {
			q.step = uint64(-(s + 1)) + 1
		} else {
			q.step = uint64(s)
		}
	}
	a, errA := strconv.ParseInt(parts[0], 10, 64)
	b, errB := strconv.ParseInt(parts[1], 10, 64)
	switch {
	case errA == nil && errB == nil:
		if (len(parts[0]) > 1 && strings.TrimLeft(parts[0], "-")[0] == '0') ||
			(len(parts[1]) > 1 && strings.TrimLeft(parts[1], "-")[0] == '0') {
			q.width = max(len(parts[0]), len(parts[1]))
		}
	case len(parts[0]) == 1 && len(parts[1]) == 1:
		a, b = int64(parts[0][0]), int64(parts[1][0])
		q.chars = true
	default:
		return nil
	}
	q.a = a
	var dist uint64
	if a <= b {
		dist = uint64(b) - uint64(a)
	} else {
		dist = uint64(a) - uint64(b)
		q.down = true
	}
	if n := dist / q.step; n >= maxExpansions {
		q.count = maxExpansions
	} else {
		q.count = int(n) + 1
	}
	return q
}

// --- segments -----------------------------------------------------------------------

// segMatcher matches one segment of a path.
type segMatcher struct {
	toks []tok
	// ext is set when the segment has an extglob; without one, the
	// matcher is the linear two-pointer one.
	ext bool
	// re matches a segment with extglobs but !(...), as minimatch
	// translates one to a RegExp; Go's RE2 runs in time linear in the
	// name and in bounded memory.
	re    *regexp.Regexp
	insts int // of re
	// neg matches a segment with one !(...), which RE2 cannot express.
	neg *negMatcher
	// dotOK is set when the pattern starts with a literal ".", so it may
	// match a name that does.
	dotOK bool
}

type tokKind uint8

const (
	tokLit   tokKind = iota
	tokAny           // ?
	tokStar          // *
	tokClass         // [...]
	tokExt           // an extglob
)

type tok struct {
	kind tokKind
	ext  byte   // '@', '?', '*', '+' or '!'
	lit  string // a run of literal characters
	cls  *charClass
	alts [][]tok // of an extglob
}

// tokBytes is what a token takes, and maxCallToks bounds the tokens one
// call parses.
const (
	tokBytes    = 64
	maxCallToks = 1 << 16
)

type charClass struct {
	neg    bool
	ranges [][2]rune
	named  []func(rune) bool
	names  []string // the POSIX classes named, for the expression
}

func (c *charClass) has(r rune) bool {
	in := false
	for _, rg := range c.ranges {
		if r >= rg[0] && r <= rg[1] {
			in = true
			break
		}
	}
	if !in {
		for _, f := range c.named {
			if f(r) {
				in = true
				break
			}
		}
	}
	return in != c.neg
}

func compileSegment(s string, cc *globCompile) (*segMatcher, error) {
	sp, err := newSegParser(s, cc)
	if err != nil {
		return nil, err
	}
	toks, err := sp.toks(0, len(s))
	if err != nil {
		return nil, err
	}
	m := &segMatcher{toks: toks}
	m.dotOK = len(toks) > 0 && toks[0].kind == tokLit && toks[0].lit[0] == '.'
	negAt := -1
	for i, t := range toks {
		if t.kind != tokExt {
			continue
		}
		m.ext = true
		if hasNeg(t.alts) {
			return nil, errTooComplex
		}
		if t.ext == '!' {
			if negAt >= 0 {
				return nil, errTooComplex
			}
			negAt = i
		}
	}
	if !m.ext {
		return m, nil
	}
	if negAt < 0 {
		re, n, err := compileRegexp(toks, cc)
		if err != nil {
			return nil, err
		}
		m.re, m.insts = re, n
		return m, nil
	}
	neg := &negMatcher{restEmpty: negAt == len(toks)-1}
	if neg.pre, neg.preInsts, err = compileRegexp(toks[:negAt], cc); err != nil {
		return nil, err
	}
	if neg.alt, neg.altInsts, err = compileRegexp([]tok{{kind: tokExt, ext: '@', alts: toks[negAt].alts}}, cc); err != nil {
		return nil, err
	}
	if neg.rest, neg.restInsts, err = compileRegexp(toks[negAt+1:], cc); err != nil {
		return nil, err
	}
	m.neg = neg
	return m, nil
}

// errTooComplex refuses the segments whose !(...) cannot be matched in
// bounded time: a second one, or one inside another extglob.
var errTooComplex = &globError{"The glob pattern is too complex: a segment may hold one !(...), outside any other extglob"}

// hasNeg reports whether a !(...) lies in the alternatives.
func hasNeg(alts [][]tok) bool {
	for _, a := range alts {
		for _, t := range a {
			if t.kind == tokExt && (t.ext == '!' || hasNeg(t.alts)) {
				return true
			}
		}
	}
	return false
}

// compileRegexp compiles tokens to an anchored expression, refusing one
// whose program passes maxRegexpInsts, and counts its instructions with
// cc. It returns the count.
func compileRegexp(toks []tok, cc *globCompile) (*regexp.Regexp, int, error) {
	var b strings.Builder
	b.WriteString("^(?:")
	writeRegexp(&b, toks)
	b.WriteString(")$")
	src := b.String()
	if len(src) > 4*maxPatternLength {
		return nil, 0, errPatternTooLong
	}
	parsed, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		return nil, 0, &globError{"The glob pattern is too complex: " + err.Error()}
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil || len(prog.Inst) > maxRegexpInsts {
		return nil, 0, errTooComplex
	}
	n := len(prog.Inst)
	if err := cc.add(n); err != nil {
		return nil, 0, err
	}
	re, err := regexp.Compile(src)
	return re, n, err
}

// writeRegexp writes the expression of tokens without !(...).
func writeRegexp(b *strings.Builder, toks []tok) {
	for i := range toks {
		t := &toks[i]
		switch t.kind {
		case tokLit:
			b.WriteString(regexp.QuoteMeta(t.lit))
		case tokAny:
			b.WriteString(`[^/]`)
		case tokStar:
			b.WriteString(`[^/]*`)
		case tokClass:
			b.WriteByte('[')
			if t.cls.neg {
				b.WriteByte('^')
			}
			for _, rg := range t.cls.ranges {
				fmt.Fprintf(b, `\x{%x}`, rg[0])
				if rg[1] != rg[0] {
					fmt.Fprintf(b, `-\x{%x}`, rg[1])
				}
			}
			for _, n := range t.cls.names {
				b.WriteString("[:" + n + ":]")
			}
			b.WriteByte(']')
		case tokExt:
			b.WriteString("(?:")
			for j, a := range t.alts {
				if j > 0 {
					b.WriteByte('|')
				}
				writeRegexp(b, a)
			}
			b.WriteByte(')')
			switch t.ext {
			case '?':
				b.WriteByte('?')
			case '*':
				b.WriteByte('*')
			case '+':
				b.WriteByte('+')
			}
		}
	}
}

// negMatcher matches a segment pre !(alt) rest: a split of the name where
// pre matches the start, alt does not match the middle, and rest matches
// the end; minimatch's !(x) also refuses a name that is x followed by
// nothing. A name has at most a few hundred bytes, but the splits are
// quadratic in it and each try runs three expressions: the work is
// counted, the runtime's interrupt is checked, and a name that takes too
// much fails the walk.
type negMatcher struct {
	pre, alt, rest                *regexp.Regexp
	preInsts, altInsts, restInsts int
	restEmpty                     bool
}

func (n *negMatcher) match(name string, meter *matchMeter) (bool, error) {
	try := func(re *regexp.Regexp, s string) (bool, error) {
		insts := n.preInsts
		switch re {
		case n.alt:
			insts = n.altInsts
		case n.rest:
			insts = n.restInsts
		}
		if err := meter.add((len(s) + 1) * (insts + 1)); err != nil {
			return false, err
		}
		return re.MatchString(s), nil
	}
	for i := 0; i <= len(name); i++ {
		if i < len(name) && !utf8.RuneStart(name[i]) {
			continue
		}
		if ok, err := try(n.pre, name[:i]); err != nil || !ok {
			if err != nil {
				return false, err
			}
			continue
		}
		whole := false
		if !n.restEmpty {
			var err error
			if whole, err = try(n.alt, name[i:]); err != nil {
				return false, err
			}
		}
		for k := i; k <= len(name); k++ {
			if k < len(name) && !utf8.RuneStart(name[k]) {
				continue
			}
			a, err := try(n.alt, name[i:k])
			if err != nil {
				return false, err
			}
			if a {
				continue
			}
			r, err := try(n.rest, name[k:])
			if err != nil {
				return false, err
			}
			if r && (n.restEmpty || !whole) {
				return true, nil
			}
		}
	}
	return false, nil
}

// maxExtDepth bounds how deep extglobs nest in a segment, and
// maxPosixName is the length of the longest POSIX class name.
const (
	maxExtDepth  = 256
	maxPosixName = 6
)

// segParser parses a segment in time linear in its length. A first pass
// finds the ")" that closes each extglob and the "|" between its
// alternatives, as a stack of the open ones does: an extglob is one when
// a ")" closes it, and its characters are read as the rest otherwise. The
// steps of a class from which it does not close are remembered, so that
// no "(" or "[" that does not close reads the rest of the segment again.
type segParser struct {
	s       string
	close   []int32 // at an extglob's first character: its ")", or -1
	bar     []int32 // at an extglob's first character and at each of its "|": the next "|", or -1
	noClass []bool  // at a step of a class: it does not close from there
	depth   int
	cc      *globCompile
}

// segParseBytes is what the first pass takes per byte of the segment.
const segParseBytes = 9

// isExtOpener reports whether an extglob begins at i.
func isExtOpener(s string, i int) bool {
	switch s[i] {
	case '@', '?', '*', '+', '!':
		return i+1 < len(s) && s[i+1] == '('
	}
	return false
}

func newSegParser(s string, cc *globCompile) (*segParser, error) {
	p := &segParser{s: s, cc: cc}
	hasClass, hasExt := strings.IndexByte(s, '[') >= 0, strings.IndexByte(s, '(') >= 0
	if !hasClass && !hasExt {
		return p, nil
	}
	if err := cc.chargeBytes(segParseBytes * len(s)); err != nil {
		return nil, err
	}
	if hasClass {
		p.noClass = make([]bool, len(s))
	}
	if !hasExt {
		return p, nil
	}
	p.close, p.bar = make([]int32, len(s)), make([]int32, len(s))
	var open, last []int32 // the extglobs open, and the last "|" of each
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ')' && len(open) > 0:
			k := len(open) - 1
			p.close[open[k]] = int32(i)
			open, last = open[:k], last[:k]
			i++
			continue
		case c == '|' && len(open) > 0:
			k := len(last) - 1
			p.bar[last[k]], p.bar[i] = int32(i), -1
			last[k] = int32(i)
			i++
			continue
		case isExtOpener(s, i):
			p.close[i], p.bar[i] = -1, -1
			open = append(open, int32(i))
			last = append(last, int32(i))
			i += 2
			continue
		case c == '\\' && i+1 < len(s):
			_, n := utf8.DecodeRuneInString(s[i+1:])
			i += 1 + n
			continue
		case c == '[':
			if _, end, ok := p.class(i+1, nil); ok {
				i = end
				continue
			}
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		i += n
	}
	return p, nil
}

// toks parses s[i:end], an alternative's characters or the segment's: a
// run of literal characters is one token.
func (p *segParser) toks(i, end int) ([]tok, error) {
	s := p.s
	var toks []tok
	litEnd := -1 // where the last token ends, when it is a run
	lit := func(from, to int) {
		if from == litEnd {
			t := &toks[len(toks)-1]
			t.lit = s[from-len(t.lit) : to]
		} else {
			toks = append(toks, tok{kind: tokLit, lit: s[from:to]})
		}
		litEnd = to
	}
	for i < end {
		c := s[i]
		if p.close != nil && isExtOpener(s, i) && p.close[i] >= 0 {
			if p.depth++; p.depth > maxExtDepth {
				return nil, errTooComplex
			}
			var alts [][]tok
			start := i + 2
			for j := int(p.bar[i]); ; j = int(p.bar[j]) {
				e := j
				if j < 0 {
					e = int(p.close[i])
				}
				alt, err := p.toks(start, e)
				if err != nil {
					return nil, err
				}
				alts = append(alts, alt)
				if j < 0 {
					break
				}
				start = j + 1
			}
			p.depth--
			toks = append(toks, tok{kind: tokExt, ext: c, alts: alts})
			litEnd = -1
			i = int(p.close[i]) + 1
			continue
		}
		switch c {
		case '\\':
			if i+1 < len(s) {
				_, n := utf8.DecodeRuneInString(s[i+1:])
				lit(i+1, i+1+n)
				i += 1 + n
				continue
			}
		case '?':
			toks = append(toks, tok{kind: tokAny})
			litEnd = -1
			i++
			continue
		case '*':
			if len(toks) == 0 || toks[len(toks)-1].kind != tokStar {
				toks = append(toks, tok{kind: tokStar})
			}
			litEnd = -1
			i++
			continue
		case '[':
			if cls, e, ok := p.class(i+1, &charClass{}); ok {
				toks = append(toks, tok{kind: tokClass, cls: cls})
				litEnd = -1
				i = e
				continue
			}
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		lit(i, i+n)
		i += n
	}
	if err := p.cc.addToks(len(toks)); err != nil {
		return nil, err
	}
	return toks, nil
}

var posixClasses = map[string]func(rune) bool{
	"alpha":  func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' },
	"digit":  func(r rune) bool { return r >= '0' && r <= '9' },
	"alnum":  func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' },
	"upper":  func(r rune) bool { return r >= 'A' && r <= 'Z' },
	"lower":  func(r rune) bool { return r >= 'a' && r <= 'z' },
	"space":  func(r rune) bool { return r == ' ' || r >= '\t' && r <= '\r' },
	"xdigit": func(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' },
	"punct": func(r rune) bool {
		return r > ' ' && r < 0x7f && !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	},
	"word": func(r rune) bool {
		return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	},
	"blank": func(r rune) bool { return r == ' ' || r == '\t' },
	"cntrl": func(r rune) bool { return r < ' ' || r == 0x7f },
	"print": func(r rune) bool { return r >= ' ' && r != 0x7f },
	"graph": func(r rune) bool { return r > ' ' && r != 0x7f },
}

// class parses a class from i, after its "[", into c (nil for none): it
// closes at the first "]" a step begins with, after the first.
func (p *segParser) class(i int, c *charClass) (*charClass, int, bool) {
	s := p.s
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		if c != nil {
			c.neg = true
		}
		i++
	}
	if i >= len(s) {
		return nil, 0, false
	}
	i = p.classStep(i, c)
	from := i
	for i < len(s) && !p.noClass[i] {
		if s[i] == ']' {
			return c, i + 1, true
		}
		i = p.classStep(i, c)
	}
	// The steps from from on lead to the end, without a "]".
	for j := from; j < len(s) && !p.noClass[j]; j = p.classStep(j, nil) {
		p.noClass[j] = true
	}
	return nil, 0, false
}

// classStep reads the item of a class at i, a POSIX class, a character or
// a range, into c (when not nil), and returns the index after it.
func (p *segParser) classStep(i int, c *charClass) int {
	s := p.s
	if s[i] == '[' && i+1 < len(s) && s[i+1] == ':' {
		if end := strings.Index(s[i+2:min(len(s), i+4+maxPosixName)], ":]"); end >= 0 {
			if f, ok := posixClasses[s[i+2:i+2+end]]; ok {
				if c != nil {
					c.named = append(c.named, f)
					c.names = append(c.names, s[i+2:i+2+end])
				}
				return i + end + 4
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
	if c != nil {
		c.ranges = append(c.ranges, [2]rune{lo, hi})
	}
	return i
}

// match reports whether the name, one segment, matches, counting its
// work with meter, which fails the match past the call's bound or at the
// runtime's interrupt.
func (m *segMatcher) match(name string, meter *matchMeter) (bool, error) {
	if name == "." || name == ".." {
		return false, nil
	}
	if strings.HasPrefix(name, ".") && !m.dotOK {
		return false, nil
	}
	switch {
	case m.neg != nil:
		return m.neg.match(name, meter)
	case m.re != nil:
		if err := meter.add((len(name) + 1) * (m.insts + 1)); err != nil {
			return false, err
		}
		return m.re.MatchString(name), nil
	}
	if err := meter.add(len(name) + len(m.toks)); err != nil {
		return false, err
	}
	return matchSimple(m.toks, name), nil
}

// single reports whether the one-character token t matches r.
func single(t *tok, r rune) bool {
	switch t.kind {
	case tokAny:
		return true
	case tokLit:
		return utf8.RuneLen(r) == len(t.lit) && string(r) == t.lit
	case tokClass:
		return t.cls.has(r)
	}
	return false
}

// matchSimple matches tokens without extglobs against all of s with the
// two-pointer algorithm: a mismatch after a star retries from one
// character further after the last star only, so the work is at most the
// product of the lengths, never exponential.
func matchSimple(toks []tok, s string) bool {
	ti, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if ti < len(toks) {
			t := &toks[ti]
			if t.kind == tokStar {
				star, mark = ti, si
				ti++
				continue
			}
			if t.kind == tokLit {
				if strings.HasPrefix(s[si:], t.lit) {
					ti++
					si += len(t.lit)
					continue
				}
			} else if r, w := utf8.DecodeRuneInString(s[si:]); single(t, r) {
				ti++
				si += w
				continue
			}
		}
		if star < 0 {
			return false
		}
		_, w := utf8.DecodeRuneInString(s[mark:])
		mark += w
		si, ti = mark, star+1
	}
	for ti < len(toks) && toks[ti].kind == tokStar {
		ti++
	}
	return ti == len(toks)
}

// --- the walk -----------------------------------------------------------------------

// globOptions are what a walk needs besides the patterns.
type globOptions struct {
	// grow charges, or holds of the budget, what the matches take as
	// they are found.
	grow           func(int) error
	followSymlinks bool
	exclude        []globPattern // matched against the paths relative to cwd, or absolute ones
	withTypes      bool
}

// globMatch is a path the walk found: as the pattern names it, relative
// to the walk's directory for a relative pattern; and the type of the
// entry.
type globMatch struct {
	path string
	typ  fs.FileMode
}

// glob walks the file system for the patterns, from cwd, a path as
// JavaScript gives it (relative to the working directory or absolute).
// The matches are sorted. Directories that cannot be read are skipped,
// as in Node.js.
func (f *fsys) glob(cwd string, patterns []globPattern, o globOptions) ([]globMatch, error) {
	w := &globWalk{f: f, o: o, seen: map[string]int{}, meter: matchMeter{halt: f.halt}}
	// Group the patterns by the directory they start at.
	starts := map[string][]int{}
	var order []string
	for i, p := range patterns {
		if _, ok := starts[p.prefix]; !ok {
			order = append(order, p.prefix)
		}
		starts[p.prefix] = append(starts[p.prefix], i)
	}
	w.patterns = patterns
	for _, prefix := range order {
		base := prefix
		if base != "" {
			if base = path.Clean(base); base == "." {
				base = ""
			}
		}
		var jsDir string
		switch {
		case strings.HasPrefix(prefix, "/"):
			jsDir = prefix
		case prefix == "":
			jsDir = cwd
		default:
			jsDir = path.Join(cwd, prefix)
		}
		rel, err := f.rel(jsDir)
		if err != nil {
			continue
		}
		var states []globState
		for _, i := range starts[prefix] {
			states = append(states, globState{pat: i})
		}
		var reals []string
		if o.followSymlinks {
			real, err := f.resolve(rel, true, false)
			if err != nil {
				continue
			}
			reals = []string{real}
		}
		w.walk(rel, base, states, reals)
	}
	if w.err == nil {
		w.err = o.grow(w.pending)
	}
	if w.err != nil {
		return nil, w.err
	}
	out := w.out
	slices.SortFunc(out, func(a, b globMatch) int { return strings.Compare(a.path, b.path) })
	return out, nil
}

type globWalk struct {
	f        *fsys
	o        globOptions
	patterns []globPattern
	out      []globMatch
	seen     map[string]int
	err      error
	pending  int // what the matches found since the last grow take
	meter    matchMeter
}

// globState is a pattern of the walk at one of its segments.
type globState struct {
	pat, seg int
}

// add records a match, once.
func (w *globWalk) add(p string, typ fs.FileMode) {
	if p == "" {
		p = "."
	}
	if w.excluded(p) {
		return
	}
	if i, ok := w.seen[p]; ok {
		if typ != 0 {
			w.out[i].typ = typ
		}
		return
	}
	w.seen[p] = len(w.out)
	w.out = append(w.out, globMatch{path: p, typ: typ})
	if w.pending += len(p) + listedOverhead; w.pending >= 64<<10 && w.err == nil {
		w.err, w.pending = w.o.grow(w.pending), 0
	}
}

// excluded reports whether an exclude pattern matches p.
func (w *globWalk) excluded(p string) bool {
	for i := range w.o.exclude {
		if w.matchPath(&w.o.exclude[i], p) {
			return true
		}
	}
	return false
}

// matchPath reports whether the pattern g matches the path p as a whole.
// A segment that fails to match (a !(...) too long, an interrupt) fails
// the walk.
func (w *globWalk) matchPath(g *globPattern, p string) bool {
	var parts []string
	if g.prefix != "" {
		pre := strings.TrimSuffix(g.prefix, "/")
		if p != pre && !strings.HasPrefix(p, pre+"/") {
			return false
		}
		p = strings.TrimPrefix(strings.TrimPrefix(p, pre), "/")
	}
	if p != "" && p != "." {
		parts = strings.Split(p, "/")
	}
	pm := pathMatch{w: w}
	return pm.segs(g.segs, parts)
}

// pathMatch matches segments against path components, remembering the
// answer for each pair of suffixes, so that "**" after "**" is not
// exponential.
type pathMatch struct {
	w    *globWalk
	memo map[[2]int]bool
}

func (pm *pathMatch) segs(segs []globSeg, parts []string) bool {
	if len(segs) == 0 {
		return len(parts) == 0
	}
	key := [2]int{len(segs), len(parts)}
	if v, ok := pm.memo[key]; ok {
		return v
	}
	v := pm.segs1(segs, parts)
	if pm.memo == nil {
		pm.memo = map[[2]int]bool{}
	}
	pm.memo[key] = v
	return v
}

func (pm *pathMatch) segs1(segs []globSeg, parts []string) bool {
	s := segs[0]
	if s.globstar {
		// A final "**" of an exclude pattern excludes what is below the
		// directory, not the directory, as in Node.js.
		first := 0
		if len(segs) == 1 {
			first = 1
		}
		for i := first; i <= len(parts); i++ {
			if i > 0 && strings.HasPrefix(parts[i-1], ".") {
				return false
			}
			if pm.segs(segs[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if s.m != nil {
		if !pm.w.test(s.m, parts[0]) {
			return false
		}
	} else if s.lit != parts[0] {
		return false
	}
	return pm.segs(segs[1:], parts[1:])
}

// test matches a segment, recording its failure as the walk's.
func (w *globWalk) test(m *segMatcher, name string) bool {
	if w.err != nil {
		return false
	}
	ok, err := m.match(name, &w.meter)
	if err != nil && w.err == nil {
		w.err = err
	}
	return ok
}

// join joins a directory of the walk, as the pattern names it, and a
// name.
func joinGlob(dir, name string) string {
	switch dir {
	case "":
		return name
	case "/":
		return "/" + name
	}
	return dir + "/" + name
}

// walk processes the states at the directory rel (relative to the root),
// which the patterns name as dir. With followSymlinks, reals are the real
// paths of dir and the directories above it in the walk, so that a link
// back to one of them is not followed again.
func (w *globWalk) walk(rel, dir string, states []globState, reals []string) {
	f := w.f
	if w.err != nil {
		return
	}
	if w.err = f.halt(); w.err != nil {
		return
	}
	// Expand the globstars that match no directory, and take the
	// patterns that end here.
	var next []globState
	var needList bool
	// The states, in a set once a globstar adds to many of them.
	var have map[globState]bool
	for i := 0; i < len(states); i++ {
		if !w.count(1) {
			return
		}
		st := states[i]
		p := &w.patterns[st.pat]
		if st.seg == len(p.segs) {
			// The pattern's prefix was all of it (a literal path).
			continue
		}
		seg := p.segs[st.seg]
		if seg.globstar {
			if st.seg == len(p.segs)-1 {
				// A final "**" matches the directory itself, "." only
				// for a pattern that is "**" alone or starts with "."
				if dir != "" || len(p.segs) == 1 {
					// (an empty dir is the walk's own directory)
					w.add(dir, fs.ModeDir)
				}
			} else {
				ns := globState{st.pat, st.seg + 1}
				switch {
				case have != nil:
					if !have[ns] {
						have[ns] = true
						states = append(states, ns)
					}
				case len(states) < 16:
					if !slices.Contains(states, ns) {
						states = append(states, ns)
					}
				default:
					have = make(map[globState]bool, 2*len(states))
					for _, x := range states {
						have[x] = true
					}
					if !have[ns] {
						have[ns] = true
						states = append(states, ns)
					}
				}
			}
			needList = true
			next = append(next, st)
			continue
		}
		if seg.m != nil {
			needList = true
		}
		next = append(next, st)
	}
	states = next
	if len(states) == 0 {
		return
	}
	var ents []dirent
	if needList {
		var err error
		if ents, err = f.readdir(rel); err != nil {
			return
		}
	}
	// The literal names are looked up, each directory among them gone
	// into with what follows; the listed names are matched one after the
	// other, and each directory gone into before the next is matched, so
	// that the walk holds the states of the directories on its path only.
	type litChild struct {
		states []globState
		typ    fs.FileMode
	}
	var lits map[string]*litChild
	var litNames []string
	var star, match []globState
	for _, st := range states {
		p := &w.patterns[st.pat]
		seg := p.segs[st.seg]
		last := st.seg == len(p.segs)-1
		switch {
		case seg.globstar:
			star = append(star, st)
		case seg.m == nil:
			// A literal: no listing, a stat of the name.
			if !w.count(1) {
				return
			}
			r := rel + "/" + seg.lit
			info, err := f.stat(r, false)
			if err != nil {
				if last && !p.dirOnly {
					// A dangling link still matches, as lstat finds it.
					if linfo, lerr := f.stat(r, true); lerr == nil {
						w.add(joinGlob(dir, seg.lit), linfo.Mode().Type())
					}
				}
				continue
			}
			if last {
				if !p.dirOnly || info.IsDir() {
					typ := info.Mode().Type()
					if linfo, err := f.stat(r, true); err == nil {
						typ = linfo.Mode().Type()
					}
					w.add(joinGlob(dir, seg.lit), typ)
				}
				continue
			}
			if info.IsDir() {
				c := lits[seg.lit]
				if c == nil {
					if lits == nil {
						lits = map[string]*litChild{}
					}
					c = &litChild{typ: fs.ModeDir}
					lits[seg.lit] = c
					litNames = append(litNames, seg.lit)
				}
				c.states = append(c.states, globState{st.pat, st.seg + 1})
			}
		default:
			match = append(match, st)
		}
	}
	var cs []globState
	for _, d := range ents {
		if !w.count(len(star) + len(match) + 1) {
			return
		}
		cs = cs[:0]
		dirLike := d.typ.IsDir() || (w.o.followSymlinks && d.typ&fs.ModeSymlink != 0)
		if len(star) > 0 && !strings.HasPrefix(d.name, ".") {
			for _, st := range star {
				if p := &w.patterns[st.pat]; st.seg == len(p.segs)-1 && (!p.dirOnly || d.typ.IsDir()) {
					w.add(joinGlob(dir, d.name), d.typ)
				}
			}
			if dirLike {
				cs = append(cs, star...)
			}
		}
		for _, st := range match {
			p := &w.patterns[st.pat]
			if !w.test(p.segs[st.seg].m, d.name) {
				continue
			}
			if st.seg == len(p.segs)-1 {
				if !p.dirOnly || d.typ.IsDir() {
					w.add(joinGlob(dir, d.name), d.typ)
				}
				continue
			}
			if dirLike {
				cs = append(cs, globState{st.pat, st.seg + 1})
			}
		}
		if c := lits[d.name]; c != nil {
			cs = append(cs, c.states...)
			delete(lits, d.name)
		}
		if len(cs) > 0 {
			w.child(rel, dir, d.name, d.typ, cs, reals)
		}
		if w.err != nil {
			return
		}
	}
	for _, name := range litNames {
		if c := lits[name]; c != nil {
			w.child(rel, dir, name, c.typ, c.states, reals)
		}
		if w.err != nil {
			return
		}
	}
}

// child walks the directory name of dir, which states go on in.
func (w *globWalk) child(rel, dir, name string, typ fs.FileMode, states []globState, reals []string) {
	f := w.f
	childRel := rel + "/" + name
	childDir := joinGlob(dir, name)
	if w.excluded(childDir) {
		return
	}
	childReals := reals
	if w.o.followSymlinks {
		var real string
		if typ&fs.ModeSymlink != 0 {
			res, err := f.resolve(childRel, true, false)
			if err != nil {
				return
			}
			if info, err := f.stat(childRel, false); err != nil || !info.IsDir() {
				return
			}
			real = res
		} else if len(reals) > 0 {
			real = path.Join(reals[len(reals)-1], name)
		}
		if slices.Contains(reals, real) {
			return // a cycle
		}
		childReals = append(slices.Clip(reals), real)
	}
	w.walk(childRel, childDir, dedupStates(states), childReals)
}

// count counts n units of the walk's work on the meter of its matches.
func (w *globWalk) count(n int) bool {
	if w.err == nil {
		w.err = w.meter.add(n)
	}
	return w.err == nil
}

func dedupStates(s []globState) []globState {
	if len(s) < 2 {
		return s
	}
	slices.SortFunc(s, func(a, b globState) int {
		if a.pat != b.pat {
			return a.pat - b.pat
		}
		return a.seg - b.seg
	})
	return slices.Compact(s)
}

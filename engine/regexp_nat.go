package engine

// Native matcher. Patterns of the RE2 class (reExact) run here on the
// subject's own code units, ASCII bytes or UTF-16 units, with no UTF-8
// working copy and no index maps: positions are code units throughout. It
// is a backtracker in JavaScript's priority order whose work is bounded by
// a visited set (BitState, as in RE2 and Go's regexp): a state (program
// point, position) is explored at most once per exec, so a search costs
// O(n·m) for a subject of n units and a program of m instructions, never
// the exponential time plain backtracking takes on `(a+)+$`.
//
// The visited set is sound because a state's outcome depends on nothing
// but the state: no backreference reads the captures, and reExact keeps
// out the two RepeatMatcher rules that look at more than the position (an
// iteration past min that matches empty fails; captures reset at each
// iteration), which on the class it admits give RE2's results, so the
// last iteration's captures and no empty iteration (every loop body
// consumes a character). The search shares the set across its start
// positions: a state that failed from one start fails from the next.
//
// Only some instructions are states: the entry, splits, runs and every
// instruction with two or more predecessors. Any other instruction has one
// predecessor and is run at most once per entry of the state before it, so
// it needs no bit. A greedy unbounded run marks the position after each
// character it takes, as the loop it stands for (L: x then L, or exit)
// would visit L there, and stops at one already visited; a lazy one marks
// each position it extends to.
//
// The set has (n - start + 1) × states bits. The backtrack stack holds at
// most m + (n + 1) × L entries for a program of m instructions, L of them
// instructions that push (splits, runs, saves) inside a loop: an attempt
// that fails unwinds what it pushed, outside loops an instruction runs at
// most once on a path, and inside one it runs again only after an
// iteration, which takes a unit at least (reExact admits no loop whose
// body can match empty). A subject for which either bound passes its cap,
// natMaxBits or natMaxStack, runs on Go's regexp instead, as before the
// native matcher (fits; initRegExpSubject decides per subject, from its
// length alone, whatever the realm's memory limit). The realm keeps a
// bitmap up to natKeepBits and a stack up to natKeepStack between execs
// and charges every one it allocates to the memory limit. The run loop
// polls the interrupt every natCheckInterval steps.

import (
	"slices"
	"unsafe"
)

type natOp uint8

const (
	natMatch    natOp = iota
	natUnit           // the code unit a (never a surrogate in u mode)
	natClass          // one character of set a
	natClassN         // exactly b characters of set a
	natStar           // greedy run of at most b (-1: unbounded) characters of set a
	natLazy           // lazy run of at most b (-1: unbounded) characters of set a
	natSplit          // continue at a, backtrack to b
	natJmp            // continue at a
	natSave           // capture slot a := position
	natBegin          // ^ (flag: multiline)
	natEnd            // $ (flag: multiline)
	natWordB          // \b (flag: U+017F and U+212A are word characters)
	natNotWordB       // \B (flag as natWordB)
)

// natInst is one instruction; state is its visited-state index, or -1.
type natInst struct {
	op    natOp
	flag  bool
	state int32
	a, b  int32
}

// natProg is a compiled pattern, immutable and shared like the RE2 program.
type natProg struct {
	insts   []natInst
	sets    []btSet
	nstates int
	nslots  int
	unicode bool
	// anchored: every match starts at 0 (a leading ^ without m).
	anchored bool
	// first is the set of the characters a match starts with, -1 when a
	// match can be empty or start with anything; firstByte its only member
	// when that is one ASCII character, else -1.
	first     int32
	firstByte int32
	// loopPush is L, the instructions inside a loop that push an entry;
	// maxLen the longest subject fits admits.
	loopPush int
	maxLen   int
}

const (
	// natMaxInsts bounds a program; a larger one runs on Go's regexp.
	natMaxInsts = 4096
	// natMaxBits bounds the visited set of one exec: 1 MiB.
	natMaxBits = 1 << 23
	// natKeepBits is the largest visited set a realm keeps between execs.
	natKeepBits = 1 << 19
	// natMaxStack bounds the backtrack stack of one exec: 1 MiB.
	natMaxStack = 1 << 20
	// natMaxEntries is natMaxStack in entries.
	natMaxEntries = natMaxStack / natEntrySize
	// natKeepStack is the largest backtrack stack a realm keeps.
	natKeepStack = 4096
	// natCheckInterval is the steps between interrupt polls.
	natCheckInterval = 4096
)

// compileNat compiles a pattern of the RE2 class, or returns nil when the
// program would pass natMaxInsts.
func compileNat(ast *reAST, flags regexpFlags) *natProg {
	c := &natCompiler{p: &natProg{nslots: 2 * len(ast.Names), unicode: flags.unicode, first: -1, firstByte: -1}}
	c.node(ast.Root)
	c.emit(natInst{op: natMatch})
	if c.tooBig {
		return nil
	}
	c.states()
	c.p.loopPush = c.loopPushes()
	c.p.maxLen = natMaxBits/c.p.nstates - 1
	if c.p.loopPush > 0 {
		c.p.maxLen = min(c.p.maxLen, (natMaxEntries-len(c.p.insts))/c.p.loopPush-1)
	}
	root := ast.Root
	for root.Op == reOpSeq && len(root.Subs) > 0 || root.Op == reOpCapture {
		root = root.Subs[0]
	}
	c.p.anchored = root.Op == reOpBegin && !root.Multiline
	if set, nullable, ok := btFirst(ast.Root); ok && !nullable && len(set) > 0 {
		c.p.first = c.set(set)
		if r, ok := set.Single(); ok && r < 0x80 {
			c.p.firstByte = r
		}
	}
	return c.p
}

type natCompiler struct {
	p      *natProg
	tooBig bool
	// loops are the first and last instructions of each loop.
	loops [][2]int32
}

// loopPushes counts the instructions inside a loop that push an entry on
// the backtrack stack.
func (c *natCompiler) loopPushes() int {
	in := make([]bool, len(c.p.insts))
	for _, l := range c.loops {
		for pc := l[0]; pc <= l[1]; pc++ {
			in[pc] = true
		}
	}
	n := 0
	for pc, inLoop := range in {
		switch c.p.insts[pc].op {
		case natSplit, natStar, natLazy, natSave:
			if inLoop {
				n++
			}
		}
	}
	return n
}

func (c *natCompiler) emit(in natInst) int32 {
	if len(c.p.insts) >= natMaxInsts {
		c.tooBig = true
		return int32(len(c.p.insts) - 1)
	}
	in.state = -1
	c.p.insts = append(c.p.insts, in)
	return int32(len(c.p.insts) - 1)
}

func (c *natCompiler) pc() int32 { return int32(len(c.p.insts)) }

func (c *natCompiler) set(set runeSet) int32 {
	c.p.sets = append(c.p.sets, newBTSet(set))
	return int32(len(c.p.sets) - 1)
}

func (c *natCompiler) node(n *reNode) {
	if c.tooBig {
		return
	}
	switch n.Op {
	case reOpEmpty:
	case reOpChar:
		if r, ok := n.Set.Single(); ok && r <= 0xFFFF && (!c.p.unicode || !isSurrogateUnit(r)) {
			c.emit(natInst{op: natUnit, a: r})
		} else {
			c.emit(natInst{op: natClass, a: c.set(n.Set)})
		}
	case reOpSeq:
		for _, s := range n.Subs {
			c.node(s)
		}
	case reOpAlt:
		var jumps []int32
		for i, s := range n.Subs {
			if i == len(n.Subs)-1 {
				c.node(s)
				break
			}
			split := c.emit(natInst{op: natSplit, a: c.pc() + 1})
			c.node(s)
			jumps = append(jumps, c.emit(natInst{op: natJmp}))
			c.p.insts[split].b = c.pc()
		}
		for _, j := range jumps {
			c.p.insts[j].a = c.pc()
		}
	case reOpCapture:
		c.emit(natInst{op: natSave, a: int32(2 * n.Index)})
		c.node(n.Subs[0])
		c.emit(natInst{op: natSave, a: int32(2*n.Index + 1)})
	case reOpRepeat:
		c.repeat(n)
	case reOpBegin:
		c.emit(natInst{op: natBegin, flag: n.Multiline})
	case reOpEnd:
		c.emit(natInst{op: natEnd, flag: n.Multiline})
	case reOpWordB:
		c.emit(natInst{op: natWordB, flag: n.Icase && c.p.unicode})
	case reOpNotWordB:
		c.emit(natInst{op: natNotWordB, flag: n.Icase && c.p.unicode})
	default: // lookarounds and backreferences are not in the RE2 class
		c.tooBig = true
	}
}

// repeat compiles a quantified atom: a character as a run instruction,
// anything else unrolled as RE2 does (min copies, then a loop or max - min
// nested optionals).
func (c *natCompiler) repeat(n *reNode) {
	body := n.Subs[0]
	if n.Max == 0 {
		return
	}
	if body.Op == reOpChar {
		set := c.set(body.Set)
		switch {
		case n.Min == 1:
			c.node(body)
		case n.Min > 1:
			c.emit(natInst{op: natClassN, a: set, b: int32(n.Min)})
		}
		if n.Max == n.Min {
			return
		}
		rest := int32(-1)
		if n.Max >= 0 {
			rest = int32(n.Max - n.Min)
		}
		op := natStar
		if !n.Greedy {
			op = natLazy
		}
		c.emit(natInst{op: op, a: set, b: rest})
		return
	}
	for range n.Min {
		if c.node(body); c.tooBig {
			return
		}
	}
	if n.Max == n.Min {
		return
	}
	if n.Max < 0 {
		loop := c.emit(natInst{op: natSplit})
		c.node(body)
		end := c.emit(natInst{op: natJmp, a: loop})
		c.branch(loop, n.Greedy)
		c.loops = append(c.loops, [2]int32{loop, end})
		return
	}
	splits := make([]int32, 0, n.Max-n.Min)
	for range n.Max - n.Min {
		splits = append(splits, c.emit(natInst{op: natSplit}))
		if c.node(body); c.tooBig {
			return
		}
	}
	for _, s := range splits {
		c.branch(s, n.Greedy)
	}
}

// branch points the split at pc into the body after it and to the end of
// the program so far, the body first when greedy.
func (c *natCompiler) branch(pc int32, greedy bool) {
	in := &c.p.insts[pc]
	if greedy {
		in.a, in.b = pc+1, c.pc()
	} else {
		in.a, in.b = c.pc(), pc+1
	}
}

// states numbers the instructions that are visited states: the entry,
// splits, runs and those with more than one predecessor.
func (c *natCompiler) states() {
	insts := c.p.insts
	preds := make([]uint8, len(insts)+1)
	for pc, in := range insts {
		switch in.op {
		case natMatch:
		case natJmp:
			preds[in.a] = min(preds[in.a]+1, 2)
		case natSplit:
			preds[in.a] = min(preds[in.a]+1, 2)
			preds[in.b] = min(preds[in.b]+1, 2)
		default:
			preds[pc+1] = min(preds[pc+1]+1, 2)
		}
	}
	n := int32(0)
	for pc := range insts {
		in := &insts[pc]
		if pc == 0 || preds[pc] > 1 || in.op == natSplit || in.op == natStar || in.op == natLazy {
			in.state = n
			n++
		}
	}
	c.p.nstates = int(n)
}

// fits reports whether a search of a subject of n units stays within
// natMaxBits and natMaxStack.
func (p *natProg) fits(n int) bool {
	return n <= p.maxLen
}

// natMachine is a realm's scratch state for the native matcher
// (regexpState.nat); a match runs no JavaScript, so one machine serves
// every exec of the realm.
type natMachine struct {
	visited []uint64
	stack   []natEntry
	caps    []int
	// budget is the steps left before the next interrupt poll.
	budget int
	// work counts the steps, the instructions run and the characters runs
	// and scans take, for the tests of the bound. natSearch keeps the budget
	// in a local and adds the steps it took when it returns or polls.
	work int64
	// visited0 and caps0 are the visited set and the slots a search starts
	// with: a pattern without captures on a few characters, the first search
	// of a plugin's initialization, allocates nothing.
	visited0 [1]uint64
	caps0    [2]int
}

// natEntry kinds.
const (
	natkJob     = iota // resume at pc, position a
	natkRestore        // slot pc := a
	natkStar           // greedy run at pc: give back one character (a: first position, b: current)
	natkLazy           // lazy run at pc: take one more character (a: position, b: count)
)

type natEntry struct {
	kind uint8
	pc   int32
	a, b int32
}

// natCodeUnit is the unit of a subject as the matcher reads it: ASCII bytes
// or UTF-16 code units.
type natCodeUnit interface{ byte | uint16 }

// exec searches sub from code unit start (exactly at start when sticky)
// and appends the match to dst: its capture positions (-1 for a group that
// did not participate), or with caps false its bounds alone. ok is false
// when there is no match, and dst is then returned as it came.
func (p *natProg) exec(r *Realm, sub *reSubject, start int, sticky, caps bool, dst []int) (out []int, ok bool, err error) {
	m := &r.regexpState().nat
	if cap(m.caps) < p.nslots {
		if m.caps = m.caps0[:]; p.nslots > len(m.caps0) {
			m.caps = make([]int, p.nslots)
		}
	}
	m.caps = m.caps[:p.nslots]
	if m.visited == nil {
		m.visited = m.visited0[:]
	}
	m.visited = m.visited[:cap(m.visited)]
	m.budget = natCheckInterval
	var s, end, hi int
	if sub.wide != nil {
		s, end, hi, err = natSearch(r, p, m, sub.wide, start, sticky, caps)
	} else {
		s, end, hi, err = natSearch(r, p, m, sub.text, start, sticky, caps)
	}
	// Every bit set is for a position in [start, hi].
	clear(m.visited[:min(len(m.visited), ((hi-start+1)*p.nstates+63)>>6)])
	if cap(m.visited) > natKeepBits/64 {
		m.visited = nil
	}
	if cap(m.stack) > natKeepStack {
		m.stack = nil
	}
	if s < 0 || err != nil {
		return dst, false, err
	}
	if !caps {
		return append(dst, s, end), true, nil
	}
	dst = slices.Grow(dst, p.nslots)
	dst = append(dst, s, end)
	for i := 2; i < p.nslots; i += 2 {
		if m.caps[i] >= 0 && m.caps[i+1] >= 0 {
			dst = append(dst, m.caps[i], m.caps[i+1])
		} else {
			dst = append(dst, -1, -1)
		}
	}
	return dst, true, nil
}

// growVisited returns m's visited set grown to hold word w, doubled but
// never past the words a search of in from start can set, the new part
// charged to the memory limit. The set grows with the positions a search
// reaches, so a search that tries few (a sticky one, or one that finds a
// match soon) allocates little whatever the subject's length.
func (m *natMachine) growVisited(r *Realm, w uint, start, n, states int) []uint64 {
	most := ((n-start+1)*states + 63) >> 6
	size := min(max(2*len(m.visited), int(w)+1, 64), most)
	r.charge(8 * size)
	v := make([]uint64, size)
	copy(v, m.visited)
	m.visited = v
	// A search that keeps no set past natKeepBits starts from visited0 again.
	m.visited0 = [1]uint64{}
	return v
}

// natScanFirst returns the first position at or after s whose character
// is in p's first set, or len(in).
func natScanFirst[E natCodeUnit](p *natProg, in []E, s int) int {
	n := len(in)
	if b := p.firstByte; b >= 0 {
		for ; s < n; s++ {
			if rune(in[s]) == rune(b) {
				return s
			}
		}
		return n
	}
	set := &p.sets[p.first]
	for s < n {
		c := rune(in[s])
		if c < 0x80 {
			if set.ascii[c>>6]&(1<<(c&63)) != 0 {
				return s
			}
			s++
			continue
		}
		c, w := natNext(in, s, p.unicode)
		if set.has(c) {
			return s
		}
		s += w
	}
	return n
}

// natNext returns the character at pos < len(in) and its width in units.
func natNext[E natCodeUnit](in []E, pos int, unicode bool) (rune, int) {
	c := rune(in[pos])
	if unicode && c >= 0xD800 && c < 0xDC00 && pos+1 < len(in) {
		if d := rune(in[pos+1]); d >= 0xDC00 && d < 0xE000 {
			return (c-0xD800)<<10 + (d - 0xDC00) + 0x10000, 2
		}
	}
	return c, 1
}

// natWidth returns the width of the character at pos < len(in).
func natWidth[E natCodeUnit](in []E, pos int, unicode bool) int {
	if unicode && pos+1 < len(in) && isHighSurrogate(rune(in[pos])) && isLowSurrogate(rune(in[pos+1])) {
		return 2
	}
	return 1
}

// natBackWidth returns the width of the character ending at pos, which a
// run took from lo.
func natBackWidth[E natCodeUnit](in []E, lo, pos int, unicode bool) int {
	if unicode && pos-2 >= lo && isLowSurrogate(rune(in[pos-1])) && isHighSurrogate(rune(in[pos-2])) {
		return 2
	}
	return 1
}

func natIsWord[E natCodeUnit](in []E, i int, fold bool) bool {
	if i < 0 || i >= len(in) {
		return false
	}
	c := rune(in[i])
	return c < 0x80 && isWordByte(byte(c)) || fold && (c == 0x17F || c == 0x212A)
}

// natSearch runs the search from start (at start only when sticky) and
// returns the start of the match (-1 for none), its end, and the highest
// position the visited set holds a bit for; bit 0 stands for start. It
// tries the start positions in order, each from the next character of the
// first set when the program has one. An attempt that fails unwinds every
// capture it saved, so the slots are reset once per search.
func natSearch[E natCodeUnit](r *Realm, p *natProg, m *natMachine, in []E, start int, sticky, caps bool) (int, int, int, error) {
	insts := p.insts
	S := p.nstates
	n := len(in)
	uni := p.unicode
	vis := m.visited
	slots := m.caps
	for i := range slots {
		slots[i] = -1
	}
	stack := m.stack[:0]
	base, s, top := start, start, start
	scan, once := p.first >= 0 && !sticky, sticky || p.anchored
	// The steps taken, and the characters scans skip, are added to work as
	// the budget is spent: credited here, debited by what is left at each
	// return.
	budget := m.budget
	m.work += int64(budget)
	var pc, pos int
next:
	if scan {
		from := s
		s = natScanFirst(p, in, s)
		budget -= s - from
		if s >= n {
			goto done // a match takes a character
		}
	}
	pc, pos = 0, s
	for {
		if budget--; budget <= 0 {
			m.work += int64(natCheckInterval - budget)
			budget = natCheckInterval
			if err := r.CheckInterrupt(); err != nil {
				m.stack, m.budget = stack[:0], budget
				m.work -= int64(budget)
				return -1, 0, top, err
			}
		}
		inst := &insts[pc]
		if inst.state >= 0 {
			bit := uint((pos-base)*S + int(inst.state))
			w, b := bit>>6, uint64(1)<<(bit&63)
			if w >= uint(len(vis)) {
				vis = m.growVisited(r, w, base, n, S)
			}
			if vis[w]&b != 0 {
				goto fail
			}
			vis[w] |= b
			top = max(top, pos)
		}
		switch inst.op {
		case natMatch:
			m.stack, m.budget = stack[:0], budget
			m.work -= int64(budget)
			return s, pos, top, nil
		case natUnit:
			if pos >= n || rune(in[pos]) != rune(inst.a) {
				goto fail
			}
			pos++
			pc++
		case natClass:
			if pos >= n {
				goto fail
			}
			set := &p.sets[inst.a]
			if c := rune(in[pos]); c < 0x80 {
				if set.ascii[c>>6]&(1<<(c&63)) == 0 {
					goto fail
				}
				pos++
			} else {
				c, w := natNext(in, pos, uni)
				if !set.has(c) {
					goto fail
				}
				pos += w
			}
			pc++
		case natClassN:
			set := &p.sets[inst.a]
			for k := int32(0); k < inst.b; k++ {
				if pos >= n {
					goto fail
				}
				if c := rune(in[pos]); c < 0x80 {
					if set.ascii[c>>6]&(1<<(c&63)) == 0 {
						goto fail
					}
					pos++
				} else {
					c, w := natNext(in, pos, uni)
					if !set.has(c) {
						goto fail
					}
					pos += w
				}
			}
			budget -= int(inst.b)
			pc++
		case natStar:
			set := &p.sets[inst.a]
			from := pos
			if inst.b < 0 {
				// Each position the run reaches enters the loop it stands
				// for again: mark it, and stop at one already entered.
				st := int(inst.state)
				for pos < n {
					c, w := rune(in[pos]), 1
					if c < 0x80 {
						if set.ascii[c>>6]&(1<<(c&63)) == 0 {
							break
						}
					} else if c, w = natNext(in, pos, uni); !set.has(c) {
						break
					}
					bit := uint((pos+w-base)*S + st)
					wd, b := bit>>6, uint64(1)<<(bit&63)
					if wd >= uint(len(vis)) {
						vis = m.growVisited(r, wd, base, n, S)
					}
					if vis[wd]&b != 0 {
						break
					}
					vis[wd] |= b
					pos += w
				}
				top = max(top, pos)
			} else {
				for k := int32(0); k < inst.b && pos < n; k++ {
					c, w := rune(in[pos]), 1
					if c < 0x80 {
						if set.ascii[c>>6]&(1<<(c&63)) == 0 {
							break
						}
					} else if c, w = natNext(in, pos, uni); !set.has(c) {
						break
					}
					pos += w
				}
			}
			budget -= pos - from
			if pos > from {
				if len(stack) == cap(stack) {
					stack = natGrow(r, stack)
				}
				stack = append(stack, natEntry{kind: natkStar, pc: int32(pc), a: int32(from), b: int32(pos)})
			}
			pc++
		case natLazy:
			if len(stack) == cap(stack) {
				stack = natGrow(r, stack)
			}
			stack = append(stack, natEntry{kind: natkLazy, pc: int32(pc), a: int32(pos)})
			pc++
		case natSplit:
			if len(stack) == cap(stack) {
				stack = natGrow(r, stack)
			}
			stack = append(stack, natEntry{kind: natkJob, pc: inst.b, a: int32(pos)})
			pc = int(inst.a)
		case natJmp:
			pc = int(inst.a)
		case natSave:
			if caps {
				if len(stack) == cap(stack) {
					stack = natGrow(r, stack)
				}
				stack = append(stack, natEntry{kind: natkRestore, pc: inst.a, a: int32(slots[inst.a])})
				slots[inst.a] = pos
			}
			pc++
		case natBegin:
			if pos != 0 && (!inst.flag || !isLineTerminatorRune(rune(in[pos-1]))) {
				goto fail
			}
			pc++
		case natEnd:
			if pos != n && (!inst.flag || !isLineTerminatorRune(rune(in[pos]))) {
				goto fail
			}
			pc++
		case natWordB, natNotWordB:
			if (natIsWord(in, pos-1, inst.flag) != natIsWord(in, pos, inst.flag)) != (inst.op == natWordB) {
				goto fail
			}
			pc++
		}
		continue
	fail:
		for {
			if len(stack) == 0 {
				if once || s >= n {
					goto done
				}
				s += natWidth(in, s, uni)
				goto next
			}
			e := &stack[len(stack)-1]
			switch e.kind {
			case natkJob:
				pc, pos = int(e.pc), int(e.a)
				stack = stack[:len(stack)-1]
			case natkRestore:
				slots[e.pc] = int(e.a)
				stack = stack[:len(stack)-1]
				continue
			case natkStar:
				cur := int(e.b)
				cur -= natBackWidth(in, int(e.a), cur, uni)
				if cur == int(e.a) {
					stack = stack[:len(stack)-1]
				} else {
					e.b = int32(cur)
				}
				pc, pos = int(e.pc)+1, cur
			case natkLazy:
				inst := &insts[e.pc]
				cur := int(e.a)
				if inst.b >= 0 && e.b >= inst.b || cur >= n {
					stack = stack[:len(stack)-1]
					continue
				}
				c, w := natNext(in, cur, uni)
				if !p.sets[inst.a].has(c) {
					stack = stack[:len(stack)-1]
					continue
				}
				cur += w
				if inst.b < 0 {
					// Extending enters the loop again at cur.
					bit := uint((cur-base)*S + int(inst.state))
					wd, b := bit>>6, uint64(1)<<(bit&63)
					if wd >= uint(len(vis)) {
						vis = m.growVisited(r, wd, base, n, S)
					}
					if vis[wd]&b != 0 {
						stack = stack[:len(stack)-1]
						continue
					}
					vis[wd] |= b
					top = max(top, cur)
				}
				e.a, e.b = int32(cur), e.b+1
				budget--
				pc, pos = int(e.pc)+1, cur
			}
			break
		}
	}
done:
	m.stack, m.budget = stack[:0], budget
	m.work -= int64(budget)
	return -1, 0, top, nil
}

// natEntrySize is the size of a backtrack stack entry.
const natEntrySize = int(unsafe.Sizeof(natEntry{}))

// natGrow doubles the backtrack stack, but not past natMaxEntries, which
// fits keeps a search's depth within, charging the new one to the memory
// limit.
func natGrow(r *Realm, stack []natEntry) []natEntry {
	n := 2*cap(stack) + 64
	if n > natMaxEntries && cap(stack) < natMaxEntries {
		n = natMaxEntries
	}
	r.charge(n * natEntrySize)
	return append(make([]natEntry, 0, n), stack...)
}

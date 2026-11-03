package engine

import (
	"sync"
	"unicode/utf16"
)

// Backtracking matcher, execution half (see regexp_bt_compile.go). The VM
// runs a btProg over the code units of the subject; positions are code-unit
// indices and, in u mode, always code-point boundaries (a surrogate pair is
// one character, a lone surrogate is a character of its own).
//
// State lives in a btMachine: the capture slots, the registers (loop
// counters, iteration starts, lookaround markers) and the backtrack stack.
// Every change a later failure must undo pushes an undo entry, and every
// alternative pushes a choice entry; failing pops entries, applying undos,
// until a choice resumes. A lookaround pushes a marker: a positive
// lookaround that matched drops the entries of its body above the marker
// except the capture undos (its body is never re-entered, the spec's
// lookarounds are atomic), a negative one unwinds to the marker.
//
// Limits: the interrupt flag is polled every btCheckInterval
// steps, and a backtrack stack above btStackLimit entries raises a
// RangeError (btStackMessage) instead of growing without bound.

const (
	btCheckInterval = 4096
	// btStackLimit bounds the backtrack stack: 1<<20 entries of 16 bytes.
	btStackLimit   = 1 << 20
	btStackMessage = "Maximum regular expression backtrack stack size exceeded"
)

// btInput is a subject as the VM reads it.
type btInput struct {
	s       string   // ASCII subject
	u       []uint16 // UTF-16 subject (wide)
	wide    bool
	n       int
	unicode bool // u mode: surrogate pairs are one character
}

func newBTInput(s *String, unicode bool) btInput {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return btInput{s: f.s, n: len(f.s), unicode: unicode}
	}
	return btInput{u: f.units(), wide: true, n: int(f.n), unicode: unicode}
}

func (in *btInput) unit(i int) rune {
	if in.wide {
		return rune(in.u[i])
	}
	return rune(in.s[i])
}

// next returns the character at i < n and its length in code units.
func (in *btInput) next(i int) (rune, int) {
	if !in.wide {
		return rune(in.s[i]), 1
	}
	c := rune(in.u[i])
	if in.unicode && c >= 0xD800 && c < 0xDC00 && i+1 < in.n {
		if d := rune(in.u[i+1]); d >= 0xDC00 && d < 0xE000 {
			return utf16.DecodeRune(c, d), 2
		}
	}
	return c, 1
}

// prev returns the character ending at i > 0 and its length in code units.
func (in *btInput) prev(i int) (rune, int) {
	if !in.wide {
		return rune(in.s[i-1]), 1
	}
	c := rune(in.u[i-1])
	if in.unicode && c >= 0xDC00 && c < 0xE000 && i >= 2 {
		if d := rune(in.u[i-2]); d >= 0xD800 && d < 0xDC00 {
			return utf16.DecodeRune(d, c), 2
		}
	}
	return c, 1
}

// isWordAt reports whether the unit at i is a word character (IsWordChar;
// every word character is one code unit). fold adds U+017F and U+212A.
func (in *btInput) isWordAt(i int, fold bool) bool {
	if i < 0 || i >= in.n {
		return false
	}
	c := in.unit(i)
	return c < 0x80 && isWordByte(byte(c)) || fold && (c == 0x17F || c == 0x212A)
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isLineTerminatorRune(c rune) bool {
	return c == '\n' || c == '\r' || c == 0x2028 || c == 0x2029
}

// btEntry kinds.
const (
	btkChoice = iota // resume at pc a, position b
	btkCap           // slot a := b
	btkReg           // register a := b
	btkLook          // lookaround marker: continue at a, position b; c 1 for a negative lookaround
	btkStar          // greedy run of instruction a: give back one character (b: position after min, c: current)
	btkLazy          // lazy run of instruction a: take one more character (b: position, c: count)
)

// btEntry is one backtrack stack entry.
type btEntry struct {
	kind, a, b, c int32
}

// btMachine is the scratch state of one exec; machines are pooled.
type btMachine struct {
	r      *Realm
	stack  []btEntry
	caps   []int
	regs   []int
	budget int
	end    int
}

var btMachines = sync.Pool{New: func() any { return new(btMachine) }}

// btPooledStack is the largest stack a pooled machine keeps.
const btPooledStack = 4096

// exec searches for the leftmost match starting at or after start (exactly
// at start when sticky) and returns its capture slots in code units (-1 for
// undefined), or nil.
func (p *btProg) exec(r *Realm, in *btInput, start int, sticky bool) ([]int, error) {
	m := btMachines.Get().(*btMachine)
	defer func() {
		m.r = nil
		if cap(m.stack) > btPooledStack {
			m.stack = nil
		}
		btMachines.Put(m)
	}()
	m.r = r
	m.budget = btCheckInterval
	if cap(m.caps) < p.nslots {
		m.caps = make([]int, p.nslots)
	}
	m.caps = m.caps[:p.nslots]
	if cap(m.regs) < p.nregs {
		m.regs = make([]int, p.nregs)
	}
	m.regs = m.regs[:p.nregs]
	for s := start; s <= in.n; {
		if p.first >= 0 && !sticky {
			set := &p.sets[p.first]
			from := s
			for s < in.n {
				c, w := in.next(s)
				if set.has(c) {
					break
				}
				s += w
			}
			if err := m.tick(s - from); err != nil {
				return nil, err
			}
			if s >= in.n {
				return nil, nil // a match consumes a character
			}
		}
		ok, err := m.run(p, in, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out := make([]int, p.nslots)
			copy(out, m.caps)
			out[0], out[1] = s, m.end
			for i := 2; i < len(out); i += 2 {
				if out[i] < 0 || out[i+1] < 0 {
					out[i], out[i+1] = -1, -1
				}
			}
			return out, nil
		}
		if sticky || p.anchored || s >= in.n {
			return nil, nil
		}
		_, w := in.next(s)
		s += w
	}
	return nil, nil
}

// tick charges k steps against the interrupt budget.
func (m *btMachine) tick(k int) error {
	if m.budget -= k; m.budget > 0 {
		return nil
	}
	m.budget = btCheckInterval
	return m.r.CheckInterrupt()
}

func (m *btMachine) push(kind int32, a, b, c int) {
	m.stack = append(m.stack, btEntry{kind, int32(a), int32(b), int32(c)})
}

// setCap and setReg change a slot or a register, pushing its undo.
func (m *btMachine) setCap(slot, v int) {
	if old := m.caps[slot]; old != v {
		m.push(btkCap, slot, old, 0)
		m.caps[slot] = v
	}
}

func (m *btMachine) setReg(reg, v int) {
	if old := m.regs[reg]; old != v {
		m.push(btkReg, reg, old, 0)
		m.regs[reg] = v
	}
}

// run matches p anchored at start. On success m.caps holds the groups and
// m.end the match end.
func (m *btMachine) run(p *btProg, in *btInput, start int) (bool, error) {
	for i := range m.caps {
		m.caps[i] = -1
	}
	m.stack = m.stack[:0]
	insts := p.insts
	pc, pos := 0, start
	for {
		if m.budget--; m.budget <= 0 {
			if err := m.tick(0); err != nil {
				return false, err
			}
		}
		if len(m.stack) > btStackLimit {
			return false, m.r.RangeError(btStackMessage)
		}
		inst := &insts[pc]
		ok := true
		switch inst.op {
		case btMatch:
			m.end = pos
			return true, nil
		case btUnit:
			switch {
			case !inst.back && pos < in.n && in.unit(pos) == rune(inst.a):
				pos++
			case inst.back && pos > 0 && in.unit(pos-1) == rune(inst.a):
				pos--
			default:
				ok = false
			}
			pc++
		case btChar:
			set := &p.sets[inst.a]
			ok = false
			if !inst.back {
				if pos < in.n {
					if c, w := in.next(pos); set.has(c) {
						pos += w
						ok = true
					}
				}
			} else if pos > 0 {
				if c, w := in.prev(pos); set.has(c) {
					pos -= w
					ok = true
				}
			}
			pc++
		case btStar:
			var k int
			if pos, k, ok = m.star(p, in, pc, pos); ok {
				pc++
			}
			m.budget -= k
		case btStarLazy:
			min := int(inst.b)
			var k int
			if pos, k = btRun(&p.sets[inst.a], in, inst.back, pos, min); k < min {
				ok = false
			} else {
				if inst.c < 0 || min < int(inst.c) {
					m.push(btkLazy, pc, pos, min)
				}
				pc++
			}
			m.budget -= k
		case btSplit:
			m.push(btkChoice, int(inst.b), pos, 0)
			pc = int(inst.a)
		case btJmp:
			pc = int(inst.a)
		case btSave:
			m.setCap(int(inst.a), pos)
			pc++
		case btBegin:
			ok = pos == 0 || inst.flag && isLineTerminatorRune(in.unit(pos-1))
			pc++
		case btEnd:
			ok = pos == in.n || inst.flag && isLineTerminatorRune(in.unit(pos))
			pc++
		case btWordB, btNotWordB:
			edge := in.isWordAt(pos-1, inst.flag) != in.isWordAt(pos, inst.flag)
			ok = edge == (inst.op == btWordB)
			pc++
		case btBackref:
			pos, ok = m.backref(p, in, inst, pos)
			pc++
		case btLookStart:
			// The register holds the marker's stack index.
			m.push(btkReg, int(inst.a), m.regs[inst.a], 0)
			m.regs[inst.a] = len(m.stack)
			neg := 0
			if inst.flag {
				neg = 1
			}
			m.push(btkLook, int(inst.b), pos, neg)
			pc++
		case btLookEnd:
			mark := m.regs[inst.a]
			if !inst.flag {
				// The body matched: restore the position and drop the body's
				// alternatives, keeping the capture undos for a later failure.
				pos = int(m.stack[mark].b)
				j := mark
				for _, e := range m.stack[mark+1:] {
					if e.kind == btkCap {
						m.stack[j] = e
						j++
					}
				}
				m.budget -= len(m.stack) - mark
				m.stack = m.stack[:j]
				pc++
				break
			}
			// The body of a negative lookaround matched: undo it and fail.
			for i := len(m.stack) - 1; i > mark; i-- {
				m.undo(m.stack[i])
			}
			m.stack = m.stack[:mark]
			ok = false
		case btLoopInit:
			m.setReg(int(inst.a), 0)
			pc++
		case btLoop:
			cnt := m.regs[inst.a]
			switch {
			case cnt < int(inst.b):
				pc++
			case inst.c >= 0 && cnt >= int(inst.c):
				pc = int(inst.d)
			case inst.flag:
				m.push(btkChoice, int(inst.d), pos, 0)
				pc++
			default:
				m.push(btkChoice, pc+1, pos, 0)
				pc = int(inst.d)
			}
		case btIterStart:
			if inst.a >= 0 {
				m.setReg(int(inst.a), pos)
			}
			for k := int(inst.b); k < int(inst.c); k++ {
				m.setCap(k, -1)
			}
			pc++
		case btIterEnd:
			cnt := m.regs[inst.a]
			if inst.b >= 0 && cnt >= int(inst.c) && pos == m.regs[inst.b] {
				ok = false
				break
			}
			m.setReg(int(inst.a), cnt+1)
			pc = int(inst.d)
		}
		if ok {
			continue
		}
		// Backtrack to the most recent choice.
		for {
			if len(m.stack) == 0 {
				return false, nil
			}
			top := len(m.stack) - 1
			e := &m.stack[top]
			switch e.kind {
			case btkChoice:
				pc, pos = int(e.a), int(e.b)
				m.stack = m.stack[:top]
			case btkLook:
				if e.c == 0 {
					m.stack = m.stack[:top]
					continue // the body of a positive lookaround failed
				}
				pc, pos = int(e.a), int(e.b) // a negative lookaround succeeded
				m.stack = m.stack[:top]
			case btkStar:
				inst := &insts[e.a]
				cur := int(e.c)
				if inst.back {
					_, w := in.next(cur)
					cur += w
				} else {
					_, w := in.prev(cur)
					cur -= w
				}
				if cur == int(e.b) {
					m.stack = m.stack[:top]
				} else {
					e.c = int32(cur)
				}
				pc, pos = int(e.a)+1, cur
			case btkLazy:
				inst := &insts[e.a]
				next, k := btRun(&p.sets[inst.a], in, inst.back, int(e.b), 1)
				if k == 0 {
					m.stack = m.stack[:top]
					continue
				}
				pc, pos = int(e.a)+1, next
				if count := int(e.c) + 1; inst.c >= 0 && count >= int(inst.c) {
					m.stack = m.stack[:top]
				} else {
					e.b, e.c = int32(next), int32(count)
				}
			default:
				m.undo(*e)
				m.stack = m.stack[:top]
				continue
			}
			break
		}
	}
}

// undo applies a capture or register undo entry.
func (m *btMachine) undo(e btEntry) {
	switch e.kind {
	case btkCap:
		m.caps[e.a] = int(e.b)
	case btkReg:
		m.regs[e.a] = int(e.b)
	}
}

// btRun consumes up to max (-1: unbounded) characters of set from pos in
// the given direction and returns the new position and the count.
func btRun(set *btSet, in *btInput, back bool, pos, max int) (int, int) {
	k := 0
	if back {
		for (max < 0 || k < max) && pos > 0 {
			c, w := in.prev(pos)
			if !set.has(c) {
				break
			}
			pos -= w
			k++
		}
		return pos, k
	}
	if !in.wide {
		for (max < 0 || k < max) && pos < in.n && set.has(rune(in.s[pos])) {
			pos++
			k++
		}
		return pos, k
	}
	for (max < 0 || k < max) && pos < in.n {
		c, w := in.next(pos)
		if !set.has(c) {
			break
		}
		pos += w
		k++
	}
	return pos, k
}

// star runs the greedy run instruction at pc: it takes as many characters
// as allowed and pushes the entry that gives them back one at a time.
func (m *btMachine) star(p *btProg, in *btInput, pc, pos int) (int, int, bool) {
	inst := &p.insts[pc]
	set := &p.sets[inst.a]
	min := int(inst.b)
	minPos, k := btRun(set, in, inst.back, pos, min)
	if k < min {
		return pos, k, false
	}
	max := int(inst.c)
	if max >= 0 {
		max -= min
	}
	end, more := btRun(set, in, inst.back, minPos, max)
	if more > 0 {
		m.push(btkStar, pc, minPos, end)
	}
	return end, k + more, true
}

// backref matches the backreference inst at pos (BackreferenceMatcher): the
// first group of its list that participated, or the empty string when none
// did.
func (m *btMachine) backref(p *btProg, in *btInput, inst *btInst, pos int) (int, bool) {
	s, e := -1, -1
	for _, g := range p.refs[inst.a : inst.a+inst.b] {
		if m.caps[2*g] >= 0 && m.caps[2*g+1] >= 0 {
			s, e = m.caps[2*g], m.caps[2*g+1]
			break
		}
	}
	if s < 0 || s == e {
		return pos, true
	}
	m.budget -= e - s
	if inst.flag {
		return btBackrefFold(in, s, e, pos, inst.back, canonicalizer(p.unicode))
	}
	n := e - s
	if !inst.back {
		if pos+n > in.n {
			return pos, false
		}
		for i := range n {
			if in.unit(s+i) != in.unit(pos+i) {
				return pos, false
			}
		}
		// u mode: the text must not end inside a surrogate pair.
		if in.unicode && pos+n < in.n && isHighSurrogate(in.unit(pos+n-1)) && isLowSurrogate(in.unit(pos+n)) {
			return pos, false
		}
		return pos + n, true
	}
	if pos-n < 0 {
		return pos, false
	}
	for i := range n {
		if in.unit(s+i) != in.unit(pos-n+i) {
			return pos, false
		}
	}
	if in.unicode && pos-n > 0 && isLowSurrogate(in.unit(pos-n)) && isHighSurrogate(in.unit(pos-n-1)) {
		return pos, false
	}
	return pos - n, true
}

// btBackrefFold compares the captured text [s, e) with the text at pos
// character by character under Canonicalize.
func btBackrefFold(in *btInput, s, e, pos int, back bool, cm *caseMap) (int, bool) {
	if !back {
		for i := s; i < e; {
			if pos >= in.n {
				return pos, false
			}
			c1, w1 := in.next(i)
			c2, w2 := in.next(pos)
			if c1 != c2 && cm.Canonicalize(c1) != cm.Canonicalize(c2) {
				return pos, false
			}
			i += w1
			pos += w2
		}
		return pos, true
	}
	for i := e; i > s; {
		if pos <= 0 {
			return pos, false
		}
		c1, w1 := in.prev(i)
		c2, w2 := in.prev(pos)
		if c1 != c2 && cm.Canonicalize(c1) != cm.Canonicalize(c2) {
			return pos, false
		}
		i -= w1
		pos -= w2
	}
	return pos, true
}

func isHighSurrogate(c rune) bool { return c >= 0xD800 && c < 0xDC00 }
func isLowSurrogate(c rune) bool  { return c >= 0xDC00 && c < 0xE000 }

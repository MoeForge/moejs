package engine

import (
	"slices"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// Array literal sites remember how large their results grew, as
// JavaScriptCore's ArrayAllocationProfile does, so that the next result of
// a site whose arrays are filled by push starts with the room they took:
// an array grown from four elements allocates the copies on the way, about
// as many bytes again as its final storage below growLargeMin and twice
// past it.
//
// A site observes the element count its result ended with (observe). The
// capacity it gives is the smaller of the last two counts, so a site whose
// sizes vary reserves no more than the smaller of its recent results
// needed, and a site that made a large array once and small ones since
// gives no more room. The capacity is all that changes: the length, holes,
// sparse transitions and every observable property of the array are those
// of the empty literal.
//
// One site of a server render defeats that bound: React's segments start
// with an empty chunks array, of which one per page takes the page's HTML,
// tens of thousands of chunks, and the others none. The smaller of the
// last two counts is zero there. A site whose results grew past siteBigMin
// becomes a candidate instead: an array growing past half of growLargeMin
// that is a candidate's latest result jumps to the smaller of the
// candidate's last two large counts (siteTable.grow), saving the copies of
// the doublings up to there.
//
// To observe a result, a site holds it, from when it makes it until the
// first of: its next result, the end of the outermost call (endJob, which
// the run's end calls while a site holds one, as it does for a queued
// job, or DropJobs after a Go panic out of the call), releaseScratch, or a
// sweep. So no result stays alive for its site
// between calls, where a site that runs once a render would otherwise keep
// what its result references, a page's elements, until the next render.
// A sweep, after the sites have made as many results as hold one and at
// least siteSweep, observes and drops the results made before the last
// sweep, but the candidates' latest results, which may still grow as the
// chunks array does: it bounds what a long call keeps to the results made
// since the last sweep and siteCands more. A large result of a site that
// is not a candidate makes it one when the sweep observes it, so the next
// result of that site is kept. A site whose results stayed within the literal's own room
// siteSettle times in a row settles: it holds and observes none, and its
// arrays are made as without feedback. A site that made one past
// siteBigMin never settles.
//
// The sites live in a table of the realm (realmLazy.sites); the shared
// function metadata, which every realm running the code reads, holds only
// the count of a function's literals (funcMeta.nlit). A function takes its
// range of the table, with the pcs of its literals, found in its code
// then, once its literals have run again sitesWarm times: a literal runs
// again at a pc no later than the last one that ran, in a loop or a later
// call. Until then the inline-cache entry past the function's own holds
// the count and the last pc, and then the range. So code that runs once,
// straight through, as a locale table's initialization does with
// thousands of literals, takes none, nor does a plugin's helper that its
// initialization calls a few times. A script's or module's own code,
// which runs once, has none, nor has dynamic code, whose caches are
// reclaimed (dynamic.go). The memory account is charged for the table as
// it grows, and the live measurement counts it with the arrays it holds.

// allocSite is the feedback of one array literal site of a realm.
type allocSite struct {
	last  *Object   // the latest result while the site holds it
	fin   [2]uint16 // the element counts of the last two results observed, latest first, at most siteFinMax
	lit   uint16    // the literal's own room
	still uint8     // results in a row that stayed within the literal's room; siteSettled once settled, siteBig once one was past siteBigMin
	gen   uint8     // the sweep generation last was made in
}

// siteTable is a realm's array literal sites.
type siteTable struct {
	s     []allocSite
	pcs   []uint32            // the pc of each site's literal, a function's in order
	held  []uint32            // the sites that hold their latest result, and settled ones that held it
	cands [siteCands]siteCand // the candidates
	next  uint32              // the candidate the next one replaces
	made  uint32              // results made since the last sweep
	gen   uint8               // the sweep generation
}

// siteCand is a candidate: a site with results past siteBigMin.
type siteCand struct {
	i   uint32    // the site's index+1; zero for none
	big [2]uint32 // the element counts of its last two results past siteBigMin, latest first; zero for none
}

const (
	// siteBigMin is the element count from which a result makes its site a
	// candidate: half of growLargeMin, where siteTable.grow looks.
	siteBigMin = growLargeMin / 2
	// siteCands is the number of candidates siteTable.grow compares.
	siteCands = 4
	// siteSettle is the number of results in a row within the literal's
	// room after which a site settles.
	siteSettle  = 4
	siteSettled = 255
	siteBig     = 254
	// sitesWarm is the number of times a function's literals run again
	// before it takes its sites. The inline-cache entry of its sites holds
	// zero before a literal ran, then up to sitesWarm, one more each time
	// they run again, with the pc of the last that ran, and once the
	// function has its range of the table, the count of its sites plus
	// sitesWarm with the range's start.
	sitesWarm = 16
	// siteSweep is the least number of results between two sweeps.
	siteSweep = 4096
	// siteRoomMax bounds the room a site gives, 16 MiB of elements.
	siteRoomMax = 1 << 20
	// siteFinMax bounds the counts a site keeps of all its results, and so
	// the room it gives at the literal: past it, siteTable.grow's room.
	siteFinMax = 1<<16 - 1
)

// literalSteps holds, for each opcode byte, the words of its instruction
// (one for an unknown opcode, as bytecode.VerifyRegisters steps), with
// literalBit set for an array literal: fillMeta counts the literals of
// every function it materializes, so the count takes one load an
// instruction and no branch.
var literalSteps = func() (t [256]uint8) {
	for i := range t {
		t[i] = 1
		if i < bytecode.OpCount {
			t[i] += uint8(bytecode.Op(i).ExtraWords())
		}
	}
	t[bytecode.NewArray] |= literalBit
	return t
}()

const literalBit = 0x80

// literalCount returns the number of code's array literals; code passed
// bytecode.VerifyRegisters.
func literalCount(code []uint32) uint32 {
	n := uint32(0)
	for pc := 0; pc < len(code); {
		s := literalSteps[uint8(code[pc])]
		pc += int(s &^ literalBit)
		n += uint32(s >> 7)
	}
	return n
}

// appendLiterals appends the pcs of code's array literals to pcs, in order.
func appendLiterals(pcs []uint32, code []uint32) []uint32 {
	for pc := 0; pc < len(code); {
		s := literalSteps[uint8(code[pc])]
		if s&literalBit != 0 {
			pcs = append(pcs, uint32(pc))
		}
		pc += int(s &^ literalBit)
	}
	return pcs
}

// newArrayAt makes the array of an array literal with room for n
// elements, or what the site's previous results took. The literal is the
// instruction of the running frame, the last, whose pc run keeps: run
// passes no more, as any value its call takes is held across every
// dispatch.
func (r *Realm) newArrayAt(n int) *Object {
	st := &r.interp
	fr := &st.frames[st.nframes-1]
	fd := fr.fn.funcData()
	if fd.meta.nlit() == 0 {
		return r.NewArrayCap(n)
	}
	t, i := r.siteOf(fd, fr.pc)
	if t == nil {
		return r.NewArrayCap(n)
	}
	s := &t.s[i]
	if s.still == siteSettled {
		return r.NewArrayCap(n)
	}
	s.lit = uint16(n)
	if l := s.last; l != nil {
		t.observe(s, i, uint32(l.elemLen()))
		if s.still == siteSettled {
			return r.NewArrayCap(n)
		}
	} else {
		r.holdSite(t, i)
	}
	o := r.NewArrayCap(max(n, int(min(s.fin[0], s.fin[1]))))
	s.last, s.gen = o, t.gen
	if t.made++; int(t.made) >= max(siteSweep, len(t.held)) {
		t.sweep()
	}
	return o
}

// siteOf returns the table and the index of the site of the literal at pc
// of fd's function, giving the function its range of the table when its
// literals run again after sitesWarm times; nil before, and for a pc that
// is none of the function's literals.
func (r *Realm) siteOf(fd *FunctionData, pc uint32) (*siteTable, uint32) {
	e := &r.ic[fd.icBase+fd.code.ICCount]
	if e.Epoch <= sitesWarm {
		if e.Epoch == 0 || pc > e.loc {
			e.Epoch, e.loc = max(e.Epoch, 1), pc
			return nil, 0
		}
		if e.Epoch < sitesWarm {
			e.Epoch, e.loc = e.Epoch+1, pc
			return nil, 0
		}
		r.bindSites(e, fd.code, fd.meta.nlit())
	}
	t := r.lazy.sites
	pcs := t.pcs[e.loc : e.loc+e.Epoch-sitesWarm]
	i, j := 0, len(pcs)
	for i < j {
		h := int(uint(i+j) >> 1)
		if pcs[h] < pc {
			i = h + 1
		} else {
			j = h
		}
	}
	if i == len(pcs) || pcs[i] != pc {
		return nil, 0
	}
	return t, e.loc + uint32(i)
}

// bindSites gives code's n literals their sites at the end of the table
// and records the range in e.
//
//go:noinline
func (r *Realm) bindSites(e *ICEntry, code *bytecode.Function, n uint32) {
	lz := r.lazyState()
	t := lz.sites
	if t == nil {
		t = &siteTable{}
		lz.sites = t
		r.charge(int(unsafe.Sizeof(siteTable{})))
	}
	e.Epoch, e.loc = n+sitesWarm, uint32(len(t.s))
	c, cp := cap(t.s), cap(t.pcs)
	t.s = slices.Grow(t.s, int(n))[:len(t.s)+int(n)]
	t.pcs = appendLiterals(slices.Grow(t.pcs, int(n)), code.Code)
	r.charge((cap(t.s)-c)*int(unsafe.Sizeof(allocSite{})) + (cap(t.pcs)-cp)*4)
}

// holdSite lists site i as holding its result; the first makes the end of
// the outermost call see it, as it sees a queued job (endJob).
func (r *Realm) holdSite(t *siteTable, i uint32) {
	if len(t.held) == cap(t.held) {
		c := cap(t.held)
		t.held = slices.Grow(t.held, 1)
		r.charge((cap(t.held) - c) * 4)
	}
	if t.held = append(t.held, i); len(t.held) == 1 {
		r.jobState()
		r.jobsPending = true
	}
}

// observe reads f, the element count the latest result of site s, index
// i, ended with.
func (t *siteTable) observe(s *allocSite, i, f uint32) {
	s.fin[1], s.fin[0] = s.fin[0], uint16(min(f, siteFinMax))
	if f >= siteBigMin {
		t.candidate(i, f)
		s.still = siteBig
	}
	switch {
	case s.still == siteBig:
	case int(f) > max(int(s.lit), 4):
		s.still = 0
	default:
		if s.still++; s.still == siteSettle {
			s.still, s.last = siteSettled, nil
		}
	}
}

// candidate records the element count f, past siteBigMin, of a result of
// site i, making the site a candidate, in place of the oldest when there
// are siteCands.
func (t *siteTable) candidate(i, f uint32) {
	for k := range &t.cands {
		if c := &t.cands[k]; c.i == i+1 {
			c.big[1], c.big[0] = c.big[0], f
			return
		}
	}
	t.cands[t.next%siteCands] = siteCand{i: i + 1, big: [2]uint32{f, 0}}
	t.next++
}

// isCandidate reports whether site i is a candidate.
func (t *siteTable) isCandidate(i uint32) bool {
	for k := range &t.cands {
		if t.cands[k].i == i+1 {
			return true
		}
	}
	return false
}

// grow returns s, of a capacity from half of growLargeMin to growLargeMin,
// with room for n more values: when s is the storage of a candidate's
// latest result, the room the candidate's last two large results took,
// the smaller, else as append grows it. The storage is recognized by its
// address, so a slice that shares it, as a mapped arguments object over
// the result's elements would, takes the same room: the capacity is all
// that differs, charged and bounded by siteRoomMax.
func (t *siteTable) grow(s []Value, n int) []Value {
	p := unsafe.SliceData(s)
	for k := range &t.cands {
		c := &t.cands[k]
		if c.i == 0 {
			continue
		}
		st := &t.s[c.i-1]
		if st.last == nil || unsafe.SliceData(st.last.elems()) != p {
			continue
		}
		room := int(c.big[0])
		if b := int(c.big[1]); b != 0 {
			room = min(room, b)
		}
		room = min(room, siteRoomMax)
		if room > 2*cap(s) {
			ns := make([]Value, len(s), max(len(s)+n, room))
			copy(ns, s)
			return ns
		}
		break
	}
	return slices.Grow(s, n)
}

// sweep observes and drops the results made before the last sweep, but
// the candidates' latest results, which may grow past siteBigMin as the
// chunks array does.
//
//go:noinline
func (t *siteTable) sweep() {
	keep := t.held[:0]
	for _, i := range t.held {
		s := &t.s[i]
		if s.last == nil {
			continue
		}
		if s.gen == t.gen || t.isCandidate(i) {
			keep = append(keep, i)
			continue
		}
		t.observe(s, i, uint32(s.last.elemLen()))
		s.last = nil
	}
	t.held = keep
	t.made = 0
	t.gen++
}

// flush observes and drops every result the sites hold: at the end of the
// outermost call (endJob) and in releaseScratch.
func (t *siteTable) flush() {
	for _, i := range t.held {
		if s := &t.s[i]; s.last != nil {
			t.observe(s, i, uint32(s.last.elemLen()))
			s.last = nil
		}
	}
	t.held = t.held[:0]
	t.made = 0
}

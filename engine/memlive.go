package engine

import (
	"errors"
	"math/bits"
	"sync"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// Live measurement (MeasureLive, RebaseAllocation).
//
// The memory limit charges what a realm allocates and credits nothing
// back (memlimit.go), so on its own it bounds what was allocated since the
// last reset. A host whose program outlives one request, an event loop
// that keeps it between callbacks, rebases the account instead of
// resetting it: RebaseAllocation measures what the realm's heap retains
// and makes that the account's base (lastLive), so that the limit bounds
// what the program keeps plus what it allocated since.
//
// The measurement walks the heap reachable from the realm's roots, the
// ones hosts registered (AddLiveRoots) and the ones the caller adds, each
// node once, and sums what the charge points charge for it: an object by
// the size of its struct and payload, the storage of its properties and
// elements, an environment by its slots, a suspended frame by its
// registers, a string by its header and units (an append buffer, and the
// string whose units a substring or a JSON value shares, once), a Map or
// Set by its table, an ArrayBuffer by its bytes (once for the buffers that
// share them), a promise by its reactions, a BigInt by its words, a shape
// of the realm's own tree with its key, the code eval and the Function
// constructors compiled. A WeakMap's value counts while both its key and
// the map are reached. The walk is iterative, so a deep structure costs no
// Go stack, and it ends early when the realm is interrupted meanwhile.
//
// What it does not count: what is shared by every realm of the process
// (the frozen intrinsics of a SharedIntrinsics realm, the shared shape
// tree, compiled templates with their string constants, compiled regular
// expressions); the intrinsic objects of a mutable realm themselves, whose
// properties it does walk, since a program can hang data on
// Array.prototype; the Go values of the host nodes FromGo made that
// JavaScript has not read yet; and every value only Go code holds: what a
// NativeFunc's closure captured, a promise only the settle functions of
// Runtime.NewPromise keep, a pending task of a host's queue. The host
// passes those as roots, or registers them (AddLiveRoots). What the Go
// state of a host's object keeps counts when that state, the object's
// payload, implements LiveReporter.

// Sizes the walk counts besides those memlimit.go declares.
const (
	objectSize   = int64(unsafe.Sizeof(Object{}))
	liveValue    = int64(valueSize)
	weakEntrySz  = int64(unsafe.Sizeof(weakEntry{}))
	collEntrySz  = int64(unsafe.Sizeof(collEntry{}))
	reactionSize = int64(unsafe.Sizeof(promiseReaction{}))
	envSize      = int64(unsafe.Sizeof(Env{}))
	// mapEntrySize is an entry of a Go map the walk does not look into
	// (a WeakMap's strong keys, the global lexical index).
	mapEntrySize = 48
)

// liveCheckEvery is how many objects and ropes the walk scans between two
// looks at the realm's interrupt.
const liveCheckEvery = 1024

// liveScanDepth is how deep the walk scans an object as it reaches it, in
// the objects scanned: one past it waits on the stack. An object scanned
// as it is reached is still in the cache.
const liveScanDepth = 8

// liveWalker is one measurement.
type liveWalker struct {
	r *Realm
	// seen holds the address of every node reached. Two nodes never share
	// one: no struct the walk marks starts with another it marks, but for
	// a dynamicImport and its reaction, of which it marks the reaction.
	seen *liveSet
	// objs and ropes are the nodes to scan.
	objs  []*Object
	ropes []*String
	// intr holds, in a mutable realm, the intrinsic objects: the walk
	// scans them but does not count them.
	intr map[*Object]struct{}
	// weak holds the WeakMap values of the keys reached before their map.
	weak map[*weakColl][]Value
	// shapes is shapeTree's stack.
	shapes []*Shape
	size   int64
	// depth is how many scans of objects reached as they were run (object).
	depth int
	// work counts the objects and ropes scanned; start is the realm's
	// interrupt payload when the walk began, which a new Interrupt
	// replaces: the walk then stops (stopped).
	work    int
	start   *interruptPayload
	stopped bool
	// add is value and addBytes counts bytes, as the functions the roots
	// and the host payloads (LiveReporter) call, made once per scratch.
	add      func(Value)
	addBytes func(n int)
}

// liveScratch is the working storage of a measurement: the set of the
// nodes reached and the walker, with its stacks of the nodes to scan. A
// realm keeps none between its measurements: a finished one leaves its
// storage, emptied, to the next measurement of any realm (liveScratches),
// and the collector frees what none takes back.
type liveScratch struct {
	set liveSet
	w   liveWalker
}

// liveScratches holds the storage of finished measurements (liveScratch).
var liveScratches sync.Pool

// liveSet is a set of the addresses of nodes, by region of 1 MiB of
// address space, found by its distance from the first region reached (a
// region far from it, a global's, goes to a map). A region holds its
// first nodes in a small hash set of their offsets and, once it holds
// more than liveSparseMax, a bitmap of one bit per 8 bytes (16 KiB): a
// heap whose nodes are dense in its address space costs a bit per 8 bytes
// of it, and nodes allocated together share words of bits, while nodes
// spread thin over the heap, among other realms' data, cost a few bytes
// each. Two nodes are always 8 bytes apart or more: none is smaller, but
// the bytes of tiny ArrayBuffers, which may share 8 bytes and then count
// once. The walk adds only nodes its roots keep alive while it runs (on
// the realm's goroutine, which alone changes the realm's heap), so no
// address is freed and reused meanwhile. It holds no pointer to a node:
// the collector neither scans the nodes through it nor keeps them alive.
type liveSet struct {
	base    uintptr                 // the region number of regions[0]
	regions []liveRegion            // by region number - base
	far     map[uintptr]*liveRegion // the regions too far from base
	// chunks, keys and slots hold the storage release emptied, for the
	// next regions: bitmaps, hash sets, and their slots by size class
	// (newSlots).
	chunks []*liveChunk
	keys   []*liveKeys
	slots  [liveSlotClasses][][]uint32
}

// liveRegion is the nodes of a region of address space in a liveSet: bits
// once it is dense, else keys (nil: none yet).
type liveRegion struct {
	bits *liveChunk
	keys *liveKeys
}

// liveKeys is the hash set of a sparse region: the offsets of its n nodes
// in the region, in units of 8 bytes, plus one (0 is a free slot), with
// linear probing, at most half full.
type liveKeys struct {
	slots []uint32 // a power of 2 long, 1 << (32 - shift)
	n     int
	shift uint32
}

const (
	liveRegionShift = 20 // 1 MiB of address space per region
	// liveRegionsMax bounds the distance of a region from base that
	// regions holds (64 GiB of address space, 1 MiB of regions).
	liveRegionsMax = 1 << 16
	// liveSparseMax is how many nodes a region holds in its hash set: one
	// more takes a bitmap, which then costs at most 32 bytes a node, where
	// the set's 1,024 slots took 4 KiB.
	liveSparseMax = 512
	// liveSlotsMin is the slots of a new hash set, liveSlotClasses the
	// sizes up to the last before the bitmap.
	liveSlotsMin    = 128
	liveSlotClasses = 4
)

// liveChunk is the bits of a dense region, one per 8 bytes.
type liveChunk [1 << (liveRegionShift - 3 - 6)]uint64

// liveOffset is the offset of a in its region, in units of 8 bytes.
func liveOffset(a uintptr) uint32 {
	return uint32(a >> 3 & (1<<(liveRegionShift-3) - 1))
}

// add sets the bit of the offset x and reports whether it was clear.
func (c *liveChunk) add(x uint32) bool {
	w, m := &c[x>>6], uint64(1)<<(x&63)
	if *w&m != 0 {
		return false
	}
	*w |= m
	return true
}

// add adds p and reports whether it was not in the set.
func (s *liveSet) add(p unsafe.Pointer) bool {
	a := uintptr(p)
	if j := a>>liveRegionShift - s.base; j < uintptr(len(s.regions)) {
		g := &s.regions[j]
		if c := g.bits; c != nil {
			return c.add(liveOffset(a))
		}
		return s.addSparse(g, liveOffset(a))
	}
	return s.addSlow(a)
}

// addSlow is add for a region out of regions.
func (s *liveSet) addSlow(a uintptr) bool {
	g := s.region(a>>liveRegionShift, true)
	if g.bits != nil {
		return g.bits.add(liveOffset(a))
	}
	return s.addSparse(g, liveOffset(a))
}

// addSparse adds the offset x to g, a region without a bitmap, and
// reports whether it was not in it.
func (s *liveSet) addSparse(g *liveRegion, x uint32) bool {
	k := g.keys
	switch {
	case k == nil:
		k = s.newKeys()
		g.keys = k
	case k.n == liveSparseMax:
		// The region is dense: its nodes move to a bitmap.
		c := s.newChunk()
		for _, y := range k.slots {
			if y != 0 {
				c.add(y - 1)
			}
		}
		s.freeKeys(k)
		g.bits, g.keys = c, nil
		return c.add(x)
	case 2*(k.n+1) > len(k.slots):
		s.growKeys(k)
	}
	slots, m := k.slots, uint32(len(k.slots)-1)
	for i := k.hash(x + 1); ; i = (i + 1) & m {
		switch slots[i] {
		case 0:
			slots[i] = x + 1
			k.n++
			return true
		case x + 1:
			return false
		}
	}
}

// hash is the first slot of the key y.
func (k *liveKeys) hash(y uint32) uint32 { return y * 0x9e3779b1 >> k.shift }

// setSlots makes slots k's, empty.
func (k *liveKeys) setSlots(slots []uint32) {
	k.slots, k.shift = slots, uint32(32-bits.TrailingZeros(uint(len(slots))))
}

// newKeys returns an empty hash set, a kept one when there is one.
func (s *liveSet) newKeys() *liveKeys {
	var k *liveKeys
	if n := len(s.keys); n != 0 {
		k = s.keys[n-1]
		s.keys = s.keys[:n-1]
	} else {
		k = new(liveKeys)
	}
	k.setSlots(s.newSlots(0))
	return k
}

// freeKeys empties k and keeps it, and its slots, for the next hash sets.
func (s *liveSet) freeKeys(k *liveKeys) {
	s.freeSlots(k.slots)
	k.slots, k.n = nil, 0
	s.keys = append(s.keys, k)
}

// growKeys doubles the slots of k.
func (s *liveSet) growKeys(k *liveKeys) {
	old := k.slots
	k.setSlots(s.newSlots(bits.TrailingZeros(uint(len(old))) - bits.TrailingZeros(liveSlotsMin) + 1))
	m := uint32(len(k.slots) - 1)
	for _, y := range old {
		if y != 0 {
			i := k.hash(y)
			for k.slots[i] != 0 {
				i = (i + 1) & m
			}
			k.slots[i] = y
		}
	}
	s.freeSlots(old)
}

// newSlots returns cleared slots of the size class class (liveSlotsMin <<
// class), kept ones when there are.
func (s *liveSet) newSlots(class int) []uint32 {
	if n := len(s.slots[class]); n != 0 {
		x := s.slots[class][n-1]
		s.slots[class] = s.slots[class][:n-1]
		return x
	}
	return make([]uint32, liveSlotsMin<<class)
}

// freeSlots clears x and keeps it for the next hash sets.
func (s *liveSet) freeSlots(x []uint32) {
	clear(x)
	class := bits.TrailingZeros(uint(len(x))) - bits.TrailingZeros(liveSlotsMin)
	s.slots[class] = append(s.slots[class], x)
}

// newChunk returns a cleared chunk, a kept one when there is one.
func (s *liveSet) newChunk() *liveChunk {
	if n := len(s.chunks); n != 0 {
		c := s.chunks[n-1]
		s.chunks = s.chunks[:n-1]
		return c
	}
	return new(liveChunk)
}

// region returns the region of the region number i, nil when the set has
// none and not create. The pointer is good until the next region made.
func (s *liveSet) region(i uintptr, create bool) *liveRegion {
	if len(s.regions) == 0 && create {
		s.base = i
	}
	n := uintptr(len(s.regions))
	switch j := i - s.base; {
	case j < n:
		return &s.regions[j]
	case j < liveRegionsMax:
		if !create {
			return nil
		}
		s.regions = append(s.regions, make([]liveRegion, j+1-n)...)
		return &s.regions[j]
	case i < s.base && s.base-i <= liveRegionsMax-n:
		if !create {
			return nil
		}
		// The regions grow downward as append grows them upward: by at
		// least as many as there are, so that a walk that reaches the
		// heap from its top moves them a few times only.
		d := min(max(s.base-i, n), liveRegionsMax-n, s.base)
		if uintptr(cap(s.regions)) >= n+d {
			s.regions = s.regions[:n+d]
			copy(s.regions[d:], s.regions[:n])
			clear(s.regions[:d])
		} else {
			grown := make([]liveRegion, n+d, 2*(n+d))
			copy(grown[d:], s.regions)
			s.regions = grown
		}
		s.base -= d
		return &s.regions[i-s.base]
	}
	g := s.far[i]
	if g == nil && create {
		if s.far == nil {
			s.far = map[uintptr]*liveRegion{}
		}
		g = new(liveRegion)
		s.far[i] = g
	}
	return g
}

// has reports whether p is in the set.
func (s *liveSet) has(p unsafe.Pointer) bool {
	a := uintptr(p)
	g := s.region(a>>liveRegionShift, false)
	switch x := liveOffset(a); {
	case g == nil:
		return false
	case g.bits != nil:
		return g.bits[x>>6]&(uint64(1)<<(x&63)) != 0
	case g.keys != nil:
		k := g.keys
		m := uint32(len(k.slots) - 1)
		for i := k.hash(x + 1); k.slots[i] != 0; i = (i + 1) & m {
			if k.slots[i] == x+1 {
				return true
			}
		}
	}
	return false
}

// release empties the set and keeps its storage, emptied, for the next
// regions.
func (s *liveSet) release() {
	for i := range s.regions {
		s.releaseRegion(&s.regions[i])
	}
	for _, g := range s.far {
		s.releaseRegion(g)
	}
	clear(s.regions)
	s.regions = s.regions[:0]
	clear(s.far)
}

func (s *liveSet) releaseRegion(g *liveRegion) {
	if g.bits != nil {
		clear(g.bits[:])
		s.chunks = append(s.chunks, g.bits)
	}
	if g.keys != nil {
		s.freeKeys(g.keys)
	}
}

// MeasureLive returns an estimate in bytes of what the realm's heap
// retains: what is reachable from the realm's roots (its global object
// and global bindings, its modules, the queued promise jobs, the targets
// WeakRef keeps alive, the tagged templates and the code eval compiled),
// from the roots hosts registered (AddLiveRoots) and from the values roots
// adds, as the charge points of the memory limit count it. roots may be
// nil. Values only Go code holds, such as the captures of a NativeFunc's
// closure, are not reached unless a host adds them. A string that shares
// the units of a longer one (a substring) counts the longer one once; a
// value JSON.parse or JSONParseGoString read out of a text does so only
// when the realm had a memory limit as it was parsed. It works with or
// without a memory limit and changes no counter (RebaseAllocation does).
// It runs on the goroutine using the realm, between calls; from a host
// function it may miss values only the Go code running holds. An
// Interrupt made while it walks ends the walk: it then returns what it
// counted so far. Called again while it measures (from roots or a
// LiveReporter), it returns 0 at once.
func (r *Realm) MeasureLive(roots func(add func(Value))) int64 {
	if m := r.mem(); m != nil {
		if m.measuring {
			return 0
		}
		m.measuring = true
		defer m.endMeasure()
	} else {
		if !liveGuard.enter(r) {
			return 0
		}
		defer liveGuard.leave(r)
	}
	size, _ := r.measureLive(roots)
	return size
}

// liveGuard holds the realms without a memory limit that are being
// measured (MeasureLive), so that a root or a LiveReporter that measures
// the realm again gets 0 at once rather than recursing; a realm with a
// limit keeps that flag in its account (memAccount.measuring).
var liveGuard realmSet

// realmSet is a set of realms that goroutines share.
type realmSet struct {
	mu     sync.Mutex
	realms map[*Realm]struct{}
}

// enter adds r and reports whether it was not in the set.
func (g *realmSet) enter(r *Realm) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.realms[r]; ok {
		return false
	}
	if g.realms == nil {
		g.realms = map[*Realm]struct{}{}
	}
	g.realms[r] = struct{}{}
	return true
}

// leave removes r.
func (g *realmSet) leave(r *Realm) {
	g.mu.Lock()
	delete(g.realms, r)
	g.mu.Unlock()
}

// measureLive is MeasureLive. It returns the size and false when an
// Interrupt ended the walk.
func (r *Realm) measureLive(roots func(add func(Value))) (int64, bool) {
	sc, _ := liveScratches.Get().(*liveScratch)
	if sc == nil {
		sc = new(liveScratch)
	}
	w := &sc.w
	if w.add == nil {
		w.add, w.addBytes = w.value, w.countBytes
	}
	w.r, w.seen, w.start = r, &sc.set, r.interruptValue.Load()
	w.realmRoots()
	if h := r.host; h != nil && h.roots != nil {
		for _, fn := range h.roots.fns {
			if fn != nil {
				fn(w.add)
			}
		}
	}
	if roots != nil {
		roots(w.add)
	}
	w.drain()
	size, ok := w.size, !w.stopped
	if m := r.mem(); m != nil {
		size += m.held
	}
	// The storage goes back emptied: the stacks keep no node alive, and
	// the walker no realm. A root that panicked leaves it to the collector.
	clear(w.objs[:cap(w.objs)])
	clear(w.ropes[:cap(w.ropes)])
	clear(w.shapes[:cap(w.shapes)])
	*w = liveWalker{objs: w.objs[:0], ropes: w.ropes[:0], shapes: w.shapes[:0], add: w.add, addBytes: w.addBytes}
	sc.set.release()
	liveScratches.Put(sc)
	return size, ok
}

// countBytes counts n bytes a host payload reports (LiveReporter).
func (w *liveWalker) countBytes(n int) {
	if n > 0 {
		w.size += int64(n)
	}
}

// RebaseAllocation makes a measurement of what the realm retains
// (MeasureLive with roots) the base of its memory account, in place of
// the allocation charged since the last reset or rebase: from then on
// the limit bounds that measurement plus what is allocated after it. A
// host that keeps a program between requests or callbacks calls it where
// it would call ResetAllocation, so that the limit bounds what the program
// keeps rather than what one request allocates.
//
// It is paced: it measures only once the allocation since the last
// measurement, resets included, reached the larger of half of what the
// limit leaves above the last measurement, (limit - LiveBytes) / 2, and
// half of the last measurement, and otherwise returns at once. A program
// that keeps little is measured once every half limit it allocates; one
// that keeps more is measured more often as it nears the limit, but never
// more than once every half of what it keeps, so that the measurements
// cost at most about twice a walk of what it keeps per such allocation.
// After a measurement past the limit (or a lower limit), and when what
// was allocated since passed it (the charges a builtin made waited, see
// below), it measures at once.
// Where that leaves an allocation passing the limit before the next
// measurement, the allocation measures first, and hits only when that
// measurement leaves no room for it: once between two calls of
// RebaseAllocation or ResetAllocation, and again each time the allocation
// since the last measurement reaches what paces this one. An allocation
// made while a builtin runs waits for it to return (remeasure).
//
// When the measurement passes the limit, it counts a hit (Stats) and
// returns its *MemoryLimitError, with Allocated 0 and Live the
// measurement; it does not interrupt the realm, whose next charge passes
// the limit again. While an interrupt is pending it does not measure; an
// Interrupt made while it measures ends the measurement, which then
// changes nothing. With no limit set it does nothing and returns nil.
func (r *Realm) RebaseAllocation(roots func(add func(Value))) error {
	if !r.StartRebase() || r.interrupted() {
		return nil
	}
	m := r.lazy.mem
	if m.measuring {
		return nil
	}
	if !r.rebase(m, roots) {
		return nil
	}
	if m.lastLive > m.limit {
		e := &MemoryLimitError{Limit: m.limit, Live: m.lastLive}
		m.hits++
		m.last = e
		return e
	}
	return nil
}

// rebase measures the heap with roots and makes the measurement the
// account's base. It reports false, changing nothing but moving the flat
// charges into the counts, when an Interrupt ended the measurement.
func (r *Realm) rebase(m *memAccount, roots func(add func(Value))) bool {
	if f := m.flat.Swap(0); f != 0 {
		m.total += f
		m.allocated += f
	}
	m.measuring = true
	defer m.endMeasure()
	live, ok := r.measureLive(roots)
	if !ok {
		return false
	}
	m.lastLive, m.allocated, m.mark, m.last = live, 0, m.total, nil
	m.pace()
	r.interruptFlag.CompareAndSwap(flagMeasure, 0) // what a pending one would do
	return true
}

// endMeasure ends a measurement, also one that a root's panic ended.
func (m *memAccount) endMeasure() { m.measuring = false }

// due reports whether the allocation since the last measurement, resets
// included, reached what paces the next: the larger of half of what the
// limit leaves above the last measurement and half of the last
// measurement.
func (m *memAccount) due() bool {
	return m.total+m.flat.Load() >= m.dueAt
}

// StartRebase is the part of RebaseAllocation that runs every time: it
// starts a new interval of the measurement an allocation past the limit
// makes, and reports whether RebaseAllocation would measure now: its
// pacing allows it, or what was allocated since the last measurement
// passed the limit. With no limit set it reports false. It is a few loads
// and a store, inlined: a host that wraps RebaseAllocation in roots of its
// own calls it first, and builds them only when it reports true.
func (r *Realm) StartRebase() bool {
	m := r.mem()
	if m == nil {
		return false
	}
	m.remeasured = false
	return m.total >= m.dueAt || m.allocated > m.room
}

// slack is how far past the limit the account may go while the
// measurement of a charge waits for a builtin to return (remeasure): half
// of what the limit leaves above the last measurement.
func (m *memAccount) slack() int64 { return max(m.room, 0) / 2 }

// remeasure's results.
const (
	notMeasured  = iota // no measurement: the charge hits
	measuredNow         // the measurement is the account's base
	measureWaits        // a builtin runs: the measurement waits for it
)

// remeasure is the measurement of a charge or a size check that would pass
// the limit (chargeSlow, overBudgetSlow): what was allocated since the last
// measurement or reset may be garbage. It measures the first time the limit
// is passed between two calls of RebaseAllocation or ResetAllocation, with
// or without an earlier measurement, and again each time the allocation
// since the last measurement reaches what paces RebaseAllocation (due), so
// that a program that makes much garbage measures once per such
// allocation, as a host's rebases would between calls.
//
// It measures only where no builtin may hold what the walk cannot reach
// (builtinRuns): a native or host function running may keep what it made
// in Go variables (the tree JSON.parse builds, a result being filled), and
// each measurement made while it runs could miss all of it. There it
// returns measureWaits: the charge stands while the account stays within
// the slack past the limit, and the interpreter measures once the native
// returns or at its next frame entry (measureAfterCall, measurePending),
// the drain between two
// promise jobs (betweenJobs) and RebaseAllocation at once; a builtin that
// allocates past the slack hits. The host's Go code outside any call, and
// its ChargeMemory in a call it holds (HoldJobs) or in a host function
// called from Go, measure at once, as RebaseAllocation does, but in
// ParseJSONString and FromGo, which hold what they build until they return
// and so wait.
// The walk takes the realm's roots and the registered ones (AddLiveRoots);
// it misses what only the host's Go code holds between calls.
func (r *Realm) remeasure(m *memAccount) int {
	return r.remeasureAt(m, atCharge)
}

// measureAt tells where a measurement is made (remeasureAt, builtinRuns).
type measureAt uint8

const (
	// atCharge: a charge, wherever the allocation is made.
	atCharge measureAt = iota
	// atCheck: the interpreter's check of the flag at a frame's entry
	// (pendingInterrupt), where a builtin the innermost frame calls is
	// calling it back.
	atCheck
	// atReturn: a native the innermost frame called has returned
	// (measureAfterCall), and its call is still counted.
	atReturn
)

// remeasureAt is remeasure made at at.
func (r *Realm) remeasureAt(m *memAccount, at measureAt) int {
	if m.remeasured && !m.due() || m.measuring || m.hitting || r.interrupted() {
		return notMeasured
	}
	if r.builtinRuns(m, at) {
		return measureWaits
	}
	m.remeasured = true
	if !r.rebase(m, nil) {
		return notMeasured
	}
	return measuredNow
}

// measurePending is the measurement a charge past the limit left to the
// interpreter (flagMeasure, pendingInterrupt), made at a frame's entry
// while what was allocated since the last measurement is past the limit;
// when the program keeps more than the limit, it hits.
func (r *Realm) measurePending() {
	m := r.mem()
	if m == nil || m.allocated <= m.room {
		return
	}
	if r.remeasureAt(m, atCheck) == measuredNow && m.lastLive > m.limit {
		r.memoryLimitHit(m)
	}
}

// measureAfterCall makes the measurement that a charge past the limit left
// pending (flagMeasure) once the native that made it returned to the
// interpreter (callValue): a loop of builtin calls measures at its next
// call, and a loop that calls nothing makes no charge that waits, so that
// a loop's back edge checks only for an interrupt. res, which only Go holds
// until the interpreter stores it, is a root. When the program keeps more
// than the limit, it hits, and the call returns the interrupt.
//
//go:noinline
func (r *Realm) measureAfterCall(res Value) error {
	if !r.interruptFlag.CompareAndSwap(flagMeasure, 0) {
		return nil
	}
	m := r.mem()
	if m == nil || m.allocated <= m.room {
		return nil
	}
	pin := r.pushArgs(1)
	pin[0] = res
	hit := r.remeasureAt(m, atReturn) == measuredNow && m.lastLive > m.limit
	r.popArgs(1)
	if !hit {
		return nil
	}
	r.memoryLimitHit(m)
	return r.interruptError()
}

// betweenJobs makes, between two promise jobs (endJob), the measurement
// a charge past the limit left waiting, which a job that ended in a
// builtin or a host function did not reach a check of the interpreter
// for: no frame runs, and no Go frame holds what the roots miss (the queue
// is among them, and the result of the call whose return drains it,
// jobState.result), so the drain is a safe point, as an event loop's
// rebase between callbacks is. It measures as remeasure does: once what
// was allocated since the last measurement passed what the limit leaves,
// and again only when due. Unlike RebaseAllocation it does not measure
// merely because the pacing allows: a host that resets the budget per task
// (ResetAllocation) has a measurement only where an allocation passes the
// limit (which the drain checks first). When the program keeps more than
// the limit, it hits.
func (r *Realm) betweenJobs(m *memAccount) {
	if m.remeasured && !m.due() || m.measuring || m.hitting || r.interrupted() {
		return
	}
	m.remeasured = true
	if r.rebase(m, nil) && m.lastLive > m.limit {
		r.memoryLimitHit(m)
	}
}

// builtinRuns reports whether a builtin may hold what a measurement made
// now cannot reach: a builtin running keeps values only in Go (holding,
// also the host's ParseJSONString and FromGo), a native may run with no
// frame below it, or a frame's instruction runs a builtin (holds). With no
// frame and no call it is the host's Go code, which a measurement takes as
// RebaseAllocation does: what it holds there it registers (AddLiveRoots).
// Within one call, the host's ChargeMemory (hostCharge) is the host's code
// still, in a call it holds (HoldJobs, as an event loop's task) or in a
// host function called from Go, which keeps what it built where the walk
// finds it as across its callbacks; the engine's own charges there may be
// a native's that a host or a promise job called. Calls that have no frame
// and run no builtin are the host's, below the frames: Runtime.RunScript,
// Load and others hold the jobs as a call.
func (r *Realm) builtinRuns(m *memAccount, at measureAt) bool {
	if m.holding != 0 {
		return true
	}
	st := &r.interp
	n := min(st.nframes, len(st.frames))
	if n == 0 {
		return r.callDepth > 1 || r.callDepth == 1 && !m.hostCharge
	}
	depth := int(r.callDepth)
	if at == atReturn {
		depth-- // the native that returned
	}
	if depth == n {
		return false // interpreted code only
	}
	for i := range n {
		if at == atReturn && i == n-1 {
			break // its call returned
		}
		var next *Object
		if i+1 < n {
			next = st.frames[i+1].fn
		}
		if r.holds(i, next, at == atCheck) {
			return true
		}
	}
	return false
}

// holds reports whether frame i's instruction runs a builtin that may keep
// values the walk cannot reach, next being the function of the frame
// above (nil for the innermost frame):
//   - a call of anything but next (a bound function's target and the
//     function f.call and f.apply call resolved) that called the frame
//     above back, unless the callee is marked measurable (a builtin that
//     keeps nothing only in Go while it calls back, or a host function);
//   - the innermost frame's call of a native, which runs now, unless the
//     interpreter's check of the flag measures (atCheck): then the call is
//     entering a frame, that of a bytecode function or one a marked
//     builtin calls back;
//   - the collection of a rest element, which keeps its values in Go
//     while the iterator's JavaScript runs.
//
// The frame above a frame whose instruction is no call was called
// implicitly (a getter, valueOf, an iterator's next): a native getter that
// builds a value is not seen (see the guide).
func (r *Realm) holds(i int, next *Object, atCheck bool) bool {
	st := &r.interp
	fr := &st.frames[i]
	if fr.fn == nil {
		return false
	}
	code := fr.fn.internal.(*FunctionData).code
	if code == nil || int(fr.pc) >= len(code.Code) {
		return false
	}
	w := code.Code[fr.pc]
	switch op := bytecode.Op(w); op {
	case bytecode.IterRest:
		return true
	case bytecode.Call, bytecode.CallSpread, bytecode.New, bytecode.NewSpread, bytecode.SuperCall, bytecode.SuperCallSpread, bytecode.CallEval:
		a := int(fr.base) + int(uint8(w>>8))
		if a+1 >= len(st.stack) || !st.stack[a].IsObject() {
			return true
		}
		callee, method := st.stack[a].AsObject(), op == bytecode.Call
		applied := false
		for range MaxCallDepth {
			fd := callee.FunctionData()
			if fd != nil && fd.kind == FuncBound {
				callee = fd.bound().target
				continue
			}
			if fd != nil && method && r.isCallOrApply(callee, fd) && st.stack[a+1].IsObject() {
				applied = fd.name == AtomApply
				callee, method = st.stack[a+1].AsObject(), false
				continue
			}
			break
		}
		if callee == next {
			return false
		}
		if applied {
			// Function.prototype.apply builds the argument list in Go
			// (CreateListFromArrayLike), whose getters and Proxy traps run
			// before the call: the frames above are theirs.
			return true
		}
		fd := callee.FunctionData()
		if next == nil && !atCheck {
			return fd == nil || fd.kind != FuncBytecode
		}
		return fd == nil || fd.kind != FuncBytecode && !fd.isMeasurable()
	}
	return false
}

// holdingBegin marks a builtin that keeps JavaScript values only in Go
// while it calls back into JavaScript (memAccount.holding), and reports
// whether it did: without a memory limit nothing measures. The builtin
// calls holdingEnd with the result when it returns.
func (r *Realm) holdingBegin() bool {
	if m := r.mem(); m != nil {
		m.holding++
		return true
	}
	return false
}

func (r *Realm) holdingEnd(began bool) {
	if m := r.mem(); began && m != nil {
		m.holding--
	}
}

// heldBase returns the bytes of Go memory the builtins running reported
// they keep while they call back into JavaScript (memAccount.held), from
// which a builtin that keeps a buffer reports its own (holdBytes), or -1
// without a memory limit, where nothing measures.
func (r *Realm) heldBase() int64 {
	if m := r.mem(); m != nil {
		return m.held
	}
	return -1
}

// holdBytes reports that the builtin that took base from heldBase keeps n
// bytes of Go memory, for the measurements made while it calls back; it
// reports 0 when it is done.
func (r *Realm) holdBytes(base int64, n int) {
	if m := r.mem(); base >= 0 && m != nil {
		m.held = base + int64(n)
	}
}

// sliceHolder returns, under a memory limit, an array that a builtin which
// builds a Go slice of values keeps on the register stack and points at
// the slice as it grows (its elements), so that the measurements made while
// the builtin calls back into JavaScript reach the values; nil without a
// limit, where nothing measures.
func (r *Realm) sliceHolder() *Object {
	if r.mem() == nil {
		return nil
	}
	return r.NewArrayFromSlice(nil)
}

// LiveReporter is implemented by the Go state a host attaches to the
// objects it creates (Object.SetInternal) or to a data function
// (NewNativeDataFunction), when that state keeps JavaScript values or Go
// memory for the object: a stream's queue, a request's body, header values
// kept as Go strings. When a measurement of the realm's heap (MeasureLive,
// RebaseAllocation, the measurement of an allocation past the memory limit)
// reaches the object, it calls ReportLive once, which adds the values the
// state keeps with add, walked as the object's own, and the bytes of Go
// memory it keeps with addBytes. What it reports counts while the object is
// reachable and stops counting once it is not, at the next measurement,
// without waiting for Go's collector. ReportLive runs on the goroutine
// using the realm, possibly in the middle of JavaScript: it must only call
// add and addBytes, without running JavaScript or creating values, and must
// not wait for a lock that is held while the realm runs JavaScript. Without
// a measurement it is never called.
//
// State reachable from a JavaScript object reports this way; AddLiveRoots
// is for what Go alone holds, which no object leads to: requests in flight,
// promises only the host's Go code will settle.
type LiveReporter interface {
	ReportLive(add func(Value), addBytes func(n int))
}

// AddLiveRoots registers roots, a function that adds values only Go code
// holds (the pending work of a host's event loop, values a host function's
// closure keeps for later) to every measurement of the realm's heap:
// MeasureLive, RebaseAllocation and the measurement an allocation past the
// memory limit makes before it hits. It returns the function that removes
// it. roots runs only while a measurement runs, on the goroutine using the
// realm and possibly in the middle of JavaScript; it must only call add,
// without running JavaScript or allocating for it, and must not wait for
// a lock that is held while the realm runs JavaScript. Without a memory
// limit nothing measures but MeasureLive. AddLiveRoots and remove run on
// the goroutine using the realm; remove may be called from roots itself.
// What a JavaScript object's Go state keeps reports through LiveReporter
// instead: a registered root counts until it is removed, reachable or not.
func (r *Realm) AddLiveRoots(roots func(add func(Value))) (remove func()) {
	if r.host == nil {
		r.host = &realmHost{}
	}
	h := r.host
	if h.roots == nil {
		h.roots = &liveRoots{}
	}
	l := h.roots
	i := 0
	for i < len(l.fns) && l.fns[i] != nil {
		i++
	}
	if i == len(l.fns) {
		l.fns = append(l.fns, nil)
	}
	l.fns[i] = roots
	done := false
	return func() {
		if !done {
			done = true
			l.fns[i] = nil
		}
	}
}

// liveRoots holds the roots hosts registered (AddLiveRoots); a removed
// one leaves nil, which the next registration takes.
type liveRoots struct {
	fns []func(add func(Value))
}

// mark records the node at p and reports whether it is new.
func (w *liveWalker) mark(p unsafe.Pointer) bool { return w.seen.add(p) }

// realmRoots adds the realm's own roots.
func (w *liveWalker) realmRoots() {
	r := w.r
	if !r.sharedIntrinsics && r.Intrinsics != nil && r.extIntrinsics != nil {
		w.intr = make(map[*Object]struct{}, 64)
		r.visitIntrinsicRoots(w.intrinsic)
	}
	w.object(r.Global)
	w.object(r.newTarget)
	for _, s := range [...]*Shape{r.plainRoot, r.nullProtoRoot, r.arrayRoot, r.funcShape} {
		w.shapeTree(s)
	}
	for _, s := range r.rootShapes {
		w.shapeTree(s)
	}
	st := &r.interp
	for _, v := range st.stack[:min(st.sp, len(st.stack))] {
		w.value(v)
	}
	for _, f := range st.frames[:min(st.nframes, len(st.frames))] {
		w.object(f.fn)
	}
	if rs := r.regexps; rs != nil {
		w.str(rs.lastS)
		for i := range rs.subjects {
			w.str(rs.subjects[i].s)
		}
	}
	lz := r.lazy
	if lz == nil {
		return
	}
	if j := lz.jobs; j != nil {
		w.value(j.result)
		if j.af != nil {
			w.asyncFrame(j.af)
		}
		w.size += int64(cap(j.kept))*liveValue + int64(cap(j.queue))*int64(unsafe.Sizeof(job{}))
		for _, v := range j.kept {
			w.value(v)
		}
		for i := j.head; i < len(j.queue); i++ {
			jb := &j.queue[i]
			w.reaction(jb.reaction)
			w.object(jb.promise)
			w.object(jb.fn)
			w.value(jb.arg)
		}
	}
	w.shapeTree(lz.argumentsShape)
	w.shapeTree(lz.mappedShape)
	if lex := lz.lex; lex != nil {
		w.size += int64(cap(lex.slots))*liveValue + int64(len(lex.index))*mapEntrySize
		for k := range lex.index {
			w.value(k.v)
		}
		for _, v := range lex.slots {
			w.value(v)
		}
	}
	if d := lz.dyn; d != nil {
		for _, root := range d.evals {
			w.code(root)
		}
		for _, root := range d.funcs {
			w.code(root)
		}
	}
	for _, o := range lz.templates {
		w.object(o)
	}
	for _, o := range lz.joins {
		w.object(o)
	}
	if mm := lz.modules; mm != nil {
		for _, m := range mm.instances {
			w.module(m)
		}
		for _, ref := range mm.refs {
			w.object(ref.meta)
		}
	}
	if s := lz.statics; s != nil {
		w.str(s.s)
	}
}

// intrinsic adds an intrinsic root of a mutable realm.
func (w *liveWalker) intrinsic(o *Object) {
	if o != nil {
		w.intr[o] = struct{}{}
		w.object(o)
	}
}

// code counts the code eval or a Function constructor compiled.
func (w *liveWalker) code(d *dynRoot) {
	if d != nil && d.code != nil && w.mark(unsafe.Pointer(d)) {
		w.size += int64(compiledSize(d.code))
	}
}

// drain scans the nodes the roots reached until none is left, or until
// an Interrupt made since the walk began stops it.
func (w *liveWalker) drain() {
	for !w.stopped {
		if n := len(w.objs); n != 0 {
			o := w.objs[n-1]
			w.objs = w.objs[:n-1]
			w.scan(o)
		} else if n := len(w.ropes); n != 0 {
			s := w.ropes[n-1]
			w.ropes = w.ropes[:n-1]
			w.tick()
			w.str(s)
		} else {
			return
		}
	}
}

// tick counts a node scanned and looks at the realm's interrupt every
// liveCheckEvery: an Interrupt made since the walk began stops it.
func (w *liveWalker) tick() {
	if w.work++; w.work%liveCheckEvery == 0 && w.r.interruptValue.Load() != w.start {
		w.stopped = true
	}
}

// value adds v: an object is queued, the other kinds counted at once.
func (w *liveWalker) value(v Value) {
	if v.ptr == nil {
		return
	}
	switch v.bits {
	case tagObject:
		w.object(v.AsObject())
	case tagString:
		w.str(v.AsString())
	case tagSymbol:
		w.symbol(v.AsSymbol())
	case tagBigInt:
		w.bigint(v.AsBigInt())
	case tagAccessor:
		a := v.asAccessor()
		if w.mark(unsafe.Pointer(a)) {
			w.size += int64(unsafe.Sizeof(Accessor{}))
			w.object(a.Get)
			w.object(a.Set)
		}
	case tagPrivate:
		w.private(v.asPrivate())
	case tagImport:
		// The exporter's binding, which its module's environment holds.
		if t := v.importTarget(); !t.isImportRef() {
			w.value(*t)
		}
	case tagAsync:
		w.asyncFrame(v.asAsyncFrame())
	}
}

// object scans o, or queues it past liveScanDepth, unless it is shared or
// was reached before.
func (w *liveWalker) object(o *Object) {
	if o == nil || o.flags&flagShared != 0 || !w.mark(unsafe.Pointer(o)) {
		return
	}
	if w.depth < liveScanDepth {
		w.depth++
		w.scan(o)
		w.depth--
		return
	}
	w.objs = append(w.objs, o)
}

// scan counts the object o and adds what it holds. An intrinsic of a
// mutable realm counts for nothing of its own, but its property values
// count: a program can hang its data on Array.prototype.
func (w *liveWalker) scan(o *Object) {
	if w.tick(); w.stopped {
		return
	}
	intr, methods := false, false
	if w.intr != nil {
		_, intr = w.intr[o]
		methods = intr || o == w.r.Global
	}
	w.object(o.proto)
	if o.shape != nil && o.shape.key == hostSentinelKey {
		// A host placeholder: its slots and internal are its Go value.
		w.size += objectSize
		return
	}
	if !intr {
		w.size += objectSize + int64(cap(o.slots)+cap(o.elements))*liveValue
		w.shape(o.shape)
	}
	for _, v := range o.slots {
		w.child(v, methods)
	}
	for _, v := range o.elements {
		w.value(v)
	}
	if d := o.dict; d != nil {
		// The root of the shapes whose prototype is o.
		w.shapeTree(d.root)
		if !intr {
			w.size += int64(len(d.entries))*int64(dictEntrySize) + int64(len(d.sparse))*int64(sparseEntrySize)
		}
		for i := range d.entries {
			if e := &d.entries[i]; e.live {
				w.value(e.key.v)
				w.child(e.cell.value, methods)
			}
		}
		for _, c := range d.sparse {
			w.value(c.value)
		}
		if d.weak != nil {
			w.weakSide(d.weak)
		}
	}
	if !intr {
		w.internal(o.internal)
	}
}

// child adds a property value of an object; methods tells that the
// object is an intrinsic or the global object of a mutable realm, whose
// native methods are intrinsics too.
func (w *liveWalker) child(v Value, methods bool) {
	if methods {
		switch {
		case v.IsObject():
			w.intrinsicMethod(v.AsObject())
		case v.isAccessor():
			a := v.asAccessor()
			w.intrinsicMethod(a.Get)
			w.intrinsicMethod(a.Set)
		}
	}
	w.value(v)
}

// intrinsicMethod marks o, a property value of an intrinsic of a mutable
// realm, an intrinsic when it is a native function not reached before.
func (w *liveWalker) intrinsicMethod(o *Object) {
	if o == nil {
		return
	}
	if fd, ok := o.internal.(*FunctionData); ok && fd.kind == FuncNative {
		if !w.seen.has(unsafe.Pointer(o)) {
			w.intr[o] = struct{}{}
		}
	}
}

// internal adds the class-specific payload of an object.
func (w *liveWalker) internal(x any) {
	switch x := x.(type) {
	case nil, *ArrayData:
	case *FunctionData:
		w.function(x)
	case *primitiveWrapper:
		w.value(x.value)
	case *String:
		w.str(x)
	case *Symbol:
		w.symbol(x)
	case *BigInt:
		w.bigint(x)
	case *argumentsMap:
		w.size += int64(unsafe.Sizeof(argumentsMap{})) + 4*int64(cap(x.slots))
		w.env(x.env)
	case *arrayBuffer:
		w.buffer(x)
	case *dataView:
		w.size += int64(unsafe.Sizeof(dataView{}))
		w.object(x.buf)
		w.buffer(x.data)
	case *typedArray:
		w.size += int64(unsafe.Sizeof(typedArray{}))
		w.object(x.view.buf)
		w.buffer(x.view.data)
	case *ErrorData:
		w.size += int64(unsafe.Sizeof(ErrorData{})) + int64(cap(x.frames))*int64(unsafe.Sizeof(StackFrame{}))
		for i := range x.frames {
			w.object(x.frames[i].Fn)
			w.object(x.frames[i].Recv)
		}
		w.value(x.stack)
	case *genFrame:
		w.size += int64(unsafe.Sizeof(genFrame{}))
		w.frame(x)
	case *asyncGenerator:
		w.size += int64(unsafe.Sizeof(asyncGenerator{})) - objectSize + int64(cap(x.queue))*int64(unsafe.Sizeof(asyncGenRequest{}))
		w.frame(&x.g)
		for i := range x.queue {
			q := &x.queue[i]
			w.value(q.value)
			w.value(q.mode)
			w.object(q.promise)
		}
	case *asyncFromSyncIter:
		w.size += int64(unsafe.Sizeof(asyncFromSyncIter{})) - objectSize
		w.value(x.it)
		w.value(x.next)
	case *IteratorData:
		w.size += int64(unsafe.Sizeof(IteratorData{}))
		w.value(x.Target)
	case *collection:
		w.size += int64(unsafe.Sizeof(collection{}))
		w.table(x.t)
	case *collIterData:
		w.size += int64(unsafe.Sizeof(collIterData{}))
		// The cursor's table leads to the collection's current one, which
		// the iterator keeps without its collection.
		for t := x.cur.t; t != nil; t = t.next {
			w.table(t)
		}
	case *weakColl:
		w.weakColl(x)
	case *weakRefData:
		w.size += int64(unsafe.Sizeof(weakRefData{}))
		w.value(x.strong)
	case *promiseData:
		w.size += int64(unsafe.Sizeof(promiseData{}))
		if x.derived {
			w.reaction(&thenPromiseOfData(x).re) // in the promise's allocation
		}
		if x.host {
			w.size += int64(unsafe.Sizeof(hostPromise{}) - unsafe.Sizeof(promiseObject{}))
		}
		w.value(x.result)
		if last := x.last; last != nil {
			for re := last.next; ; re = re.next {
				w.reaction(re)
				if re == last || re == nil {
					break
				}
			}
		}
	case *proxyData:
		w.size += int64(unsafe.Sizeof(proxyData{}))
		w.object(x.target)
		w.object(x.handler)
	case *RegExpData:
		w.size += int64(unsafe.Sizeof(RegExpData{}))
		w.str(x.source)
		w.str(x.flags)
	case *regexpStringIter:
		w.size += int64(unsafe.Sizeof(regexpStringIter{}))
		w.object(x.matcher)
		w.str(x.s)
	case *moduleNamespace:
		w.size += int64(unsafe.Sizeof(moduleNamespace{})) + int64(cap(x.cells))*8
		for _, c := range x.cells {
			if c != nil {
				w.value(*c)
			}
		}
	case *intlLocale:
		// What its construction charges: the subtags the tag copied, and
		// the [[Locale]] and keyword strings.
		w.size += int64(unsafe.Sizeof(intlLocale{}))
		if x.tag != nil {
			w.size += int64(x.tag.size())
		}
		w.str(x.str)
		for _, s := range x.kw {
			w.str(s)
		}
	case *intlNumberFormat:
		w.size += int64(x.size())
		w.str(x.locale)
		w.str(x.nuString)
		w.str(x.currencyString)
		w.str(x.unitString)
	case *intlDateTimeFormat:
		w.size += int64(unsafe.Sizeof(intlDateTimeFormat{}))
		w.str(x.locale)
		w.str(x.numbering)
		w.str(x.zoneID)
	case *intlPluralRulesData:
		w.size += int64(unsafe.Sizeof(intlPluralRulesData{}))
		if x.nf.compact != nil {
			w.size += int64(unsafe.Sizeof(intlCompactData{}))
		}
		w.str(x.locale)
	case *intlSegmenter:
		w.size += int64(unsafe.Sizeof(intlSegmenter{}))
		w.str(x.locale)
	case *intlSegments:
		w.size += int64(unsafe.Sizeof(intlSegments{}))
		w.str(x.str)
	default:
		// A host's payload reports what it keeps; anything else holds no
		// JavaScript value the walk can see: a date, a text decoder, the
		// lazy state of a builtin, a host node's Go map.
		if lr, ok := x.(LiveReporter); ok {
			lr.ReportLive(w.add, w.addBytes)
		}
	}
}

// function adds what a function holds besides its object.
func (w *liveWalker) function(fd *FunctionData) {
	w.size += int64(unsafe.Sizeof(FunctionData{}))
	w.env(fd.env)
	w.value(fd.thisValue)
	w.str(fd.name)
	switch d := fd.data.(type) {
	case *Object:
		w.object(d)
	case *boundFunc:
		w.size += int64(unsafe.Sizeof(boundFunc{})) + int64(cap(d.args))*liveValue
		w.object(d.target)
		for _, v := range d.args {
			w.value(v)
		}
	case *proxyData:
		w.object(d.target)
		w.object(d.handler)
	case *resolvingFns:
		// Both functions are the allocation, which either keeps: the other
		// one counts as an object of its own, and the resolve function's
		// thisValue the promise. d is not marked: it is where the resolve
		// function is, whose mark the objects' marks already make.
		w.object(&d.resolveFn.obj)
		w.object(&d.rejectFn.obj)
	case *capabilityExecutor:
		if w.mark(unsafe.Pointer(d)) {
			w.size += int64(unsafe.Sizeof(capabilityExecutor{}))
			w.value(d.resolve)
			w.value(d.reject)
		}
	case *finallyData:
		if w.mark(unsafe.Pointer(d)) {
			w.size += int64(unsafe.Sizeof(finallyData{}))
			w.object(d.onFinally)
			w.object(d.c)
		}
	case *Value:
		w.size += liveValue
		w.value(*d)
	case *combineElement:
		w.size += int64(unsafe.Sizeof(combineElement{}))
		if s := d.state; s != nil && w.mark(unsafe.Pointer(s)) {
			w.size += int64(unsafe.Sizeof(combineState{})) + int64(cap(s.values))*liveValue
			for _, v := range s.values {
				w.value(v)
			}
			w.capability(&s.cap)
		}
	case LiveReporter: // the payload of a host's data function
		d.ReportLive(w.add, w.addBytes)
	}
}

// asyncFrame adds the state of an async function call that awaited, with
// its reaction record and the registers allocated with it.
func (w *liveWalker) asyncFrame(af *asyncFrame) {
	if !w.mark(unsafe.Pointer(af)) {
		return
	}
	w.size += int64(unsafe.Sizeof(asyncFrame{}))
	w.frame(&af.g)
	w.object(af.promise)
}

// frame adds what a suspended generator or async frame holds.
func (w *liveWalker) frame(g *genFrame) {
	w.object(g.fn)
	w.value(g.this)
	w.env(g.env)
	w.value(g.out)
	w.size += int64(cap(g.regs)) * liveValue
	for _, v := range g.regs {
		w.value(v)
	}
}

// env adds an environment and the ones around it.
func (w *liveWalker) env(e *Env) {
	for ; e != nil && w.mark(unsafe.Pointer(e)); e = e.parent {
		w.size += envSize + int64(cap(e.slots))*liveValue
		for _, v := range e.slots {
			w.value(v)
		}
	}
}

// shapeTree counts the shapes of the realm's own tree below the root
// shape root that its strong transitions keep (the first
// shapeStrongTransitions children of each), with their keys: the realm
// keeps them whether an object has them or not. The weak tier, which dies
// with the objects, is not walked; a root is walked once.
func (w *liveWalker) shapeTree(root *Shape) {
	// The walked roots are marked at their proto field, where no node
	// starts.
	if root == nil || root.shared || root.isDict || root.key == hostSentinelKey || !w.mark(unsafe.Pointer(&root.proto)) {
		return
	}
	stack := append(w.shapes[:0], root)
	// A tree whose prototype the objects below lead to (scan) is walked
	// with a stack of its own.
	w.shapes = nil
	for len(stack) != 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		// One an object led to was counted, but not what is below it.
		if w.mark(unsafe.Pointer(s)) {
			w.size += int64(shapeSize)
			w.value(s.key.v)
			w.object(s.proto)
		}
		for _, t := range s.transSmall {
			stack = append(stack, t.shape)
		}
		for _, c := range s.transMap {
			stack = append(stack, c)
		}
	}
	clear(stack[:cap(stack)])
	w.shapes = stack[:0]
}

// shape counts the shapes of the realm's own tree from s to its root.
func (w *liveWalker) shape(s *Shape) {
	for ; s != nil && !s.shared && !s.isDict && s.key != hostSentinelKey && w.mark(unsafe.Pointer(s)); s = s.parent {
		w.size += int64(shapeSize)
		w.value(s.key.v)
		w.object(s.proto)
	}
}

// capability adds a PromiseCapability record.
func (w *liveWalker) capability(c *promiseCapability) {
	w.object(c.promise)
	w.object(c.resolve)
	w.object(c.reject)
}

// reaction adds a reaction of a pending promise or a queued job.
func (w *liveWalker) reaction(re *promiseReaction) {
	if re == nil || !w.mark(unsafe.Pointer(re)) {
		return
	}
	w.size += reactionSize
	w.capability(&re.cap)
	w.object(re.onFulfilled)
	w.object(re.onRejected)
	switch x := re.reactor.(type) {
	case *asyncFrame:
		if re == &x.re {
			w.size -= reactionSize // the record of its awaits, counted with it
		}
		w.asyncFrame(x)
	case *asyncGenerator:
		if re == &x.re {
			w.size -= reactionSize
		}
		w.object(&x.obj)
	case *asyncFromSyncStep:
		// x starts with its own reaction, which marks it.
		w.size += int64(unsafe.Sizeof(asyncFromSyncStep{}) - unsafe.Sizeof(promiseReaction{}))
		if x.sync != nil {
			w.object(&x.sync.obj)
		}
		w.object(x.promise)
	case *fromAsync:
		if w.mark(unsafe.Pointer(x)) {
			w.size += int64(unsafe.Sizeof(fromAsync{}))
			w.object(x.promise)
			w.object(x.mapper)
			w.object(x.a)
			w.object(x.arrayLike)
			w.value(x.thisArg)
			w.value(x.it)
			w.value(x.next)
			w.errValue(x.err)
		}
	case *moduleInstance:
		w.module(x)
	case *dynamicImport:
		// x starts with its own reaction, which marks it.
		w.size += int64(unsafe.Sizeof(dynamicImport{}) - unsafe.Sizeof(promiseReaction{}))
		w.reaction(&x.re)
		w.object(x.promise)
		w.str(x.specifier)
	}
}

// module adds a module instance. Its dependencies are instances of the
// same module map, which the roots add.
func (w *liveWalker) module(m *moduleInstance) {
	if m == nil || !w.mark(unsafe.Pointer(m)) {
		return
	}
	w.size += int64(unsafe.Sizeof(moduleInstance{})) + int64(cap(m.deps)+cap(m.asyncParents))*8
	w.object(m.fn)
	w.env(m.env)
	w.value(m.ns)
	w.object(m.capability)
	w.object(m.body)
	w.errValue(m.err)
}

// errValue adds the thrown value an error carries.
func (w *liveWalker) errValue(err error) {
	var ex *Exception
	if err != nil && errors.As(err, &ex) {
		w.value(ex.Value)
	}
}

// table adds a Map or Set table.
func (w *liveWalker) table(t *collTable) {
	if t == nil || !w.mark(unsafe.Pointer(t)) {
		return
	}
	w.size += int64(unsafe.Sizeof(collTable{})) + int64(cap(t.entries))*collEntrySz + 4*int64(cap(t.buckets)+cap(t.removed))
	for i := range t.entries {
		if e := &t.entries[i]; !e.key.IsHole() {
			w.value(e.key)
			w.value(e.value)
		}
	}
}

// weakColl adds a WeakMap's or WeakSet's own entries and the values of
// the keys reached before it.
func (w *liveWalker) weakColl(c *weakColl) {
	if !w.mark(unsafe.Pointer(c)) {
		return
	}
	w.size += int64(unsafe.Sizeof(weakColl{})) + int64(len(c.strong))*mapEntrySize
	for k, v := range c.strong {
		w.value(k)
		w.value(v)
	}
	if vs, ok := w.weak[c]; ok {
		delete(w.weak, c)
		for _, v := range vs {
			w.value(v)
		}
	}
}

// weakSide adds the WeakMap entries a reached key carries: the value of a
// map reached already, and keeps the others until their map is.
func (w *liveWalker) weakSide(s *weakSide) {
	w.size += int64(cap(s.entries)) * weakEntrySz
	for i := range s.entries {
		e := &s.entries[i]
		c := e.owner.Value()
		if c == nil || e.value.ptr == nil {
			continue
		}
		if w.seen.has(unsafe.Pointer(c)) {
			w.value(e.value)
			continue
		}
		if w.weak == nil {
			w.weak = make(map[*weakColl][]Value)
		}
		w.weak[c] = append(w.weak[c], e.value)
	}
}

// buffer counts an ArrayBuffer's bytes, once for the buffers sharing them.
func (w *liveWalker) buffer(b *arrayBuffer) {
	if b == nil || !w.mark(unsafe.Pointer(b)) {
		return
	}
	w.size += int64(unsafe.Sizeof(arrayBuffer{}))
	if cap(b.data) != 0 && w.mark(unsafe.Pointer(unsafe.SliceData(b.data))) {
		w.size += int64(cap(b.data))
	}
}

// str counts a string: its header and its units, once for the strings of
// an append buffer and for the strings that share another's units (its
// share root, which counts them). A rope not flattened queues its
// children. A lazy string counts its block, its source or share root, and
// its decoded storage, as decoded until it is. A constant of shared
// compiled code counts for nothing.
func (w *liveWalker) str(s *String) {
	if s == nil || s.flags&strConst != 0 || !w.mark(unsafe.Pointer(s)) {
		return
	}
	if s.flags&strCharged != 0 {
		w.size += int64(unsafe.Sizeof(ropeNode{}))
	} else {
		w.size += int64(stringHeaderSize)
	}
	switch s.kind {
	case strASCII:
		switch {
		case s.flags&strInBuf != 0:
			w.appendBuf(s, 1)
		case s.right != nil:
			w.str(s.right)
		default:
			w.size += int64(len(s.s))
		}
	case strUTF16:
		switch {
		case s.flags&strInBuf != 0:
			w.appendBuf(s, 2)
		case s.right != nil:
			w.str(s.right)
		default:
			w.size += 2 * int64(s.n)
		}
	case strRope:
		// Another goroutine may flatten it meanwhile (flatten).
		if f, ok := s.published(); ok {
			if f.kind == strASCII {
				w.size += int64(f.n)
			} else {
				w.size += 2 * int64(f.n)
			}
			return
		}
		if l, r := s.children(); l != nil {
			w.ropes = append(w.ropes, l, r)
		}
	case strLazy:
		// Its data, its source (or the text it was parsed from, its share
		// root) and what it decoded to, which another goroutine may be
		// publishing meanwhile (resolved). A host's Go text that decodes
		// to UTF-16 is the host's, uncounted as main's conversion left it:
		// only the units it decodes to count.
		w.size += int64(s.lazyBlockSize() - stringHeaderSize)
		switch root := s.lazyShareRoot(); {
		case root != nil:
			w.str(root)
		case s.lazyMode() == lazyText && s.lazyWide():
		default:
			w.size += int64(len(s.s))
		}
		f := s.lazyPublished()
		switch {
		case f == nil:
			// Not decoded yet: counted as decoded, as the eager
			// conversion would be, so a measurement keeps what the
			// string was charged (accountLazy).
			w.size += int64(s.lazyDecodedSize())
		case f.kind == strASCII && f.flags&strInBuf == 0 && unsafe.StringData(f.s) == unsafe.StringData(s.s):
			if w.mark(unsafe.Pointer(f)) { // its units are the source
				w.size += int64(stringHeaderSize)
			}
		default:
			w.str(f)
		}
	}
}

// appendBuf counts the append buffer s starts once, units of size bytes.
func (w *liveWalker) appendBuf(s *String, size int) {
	h := s.appendBuf()
	if w.mark(unsafe.Pointer(h)) {
		w.size += int64(appendBufSize + size*int(h.cap))
	}
}

// symbol counts a symbol not shared by the process, and the WeakMap
// entries it carries as a key.
func (w *liveWalker) symbol(s *Symbol) {
	if s == nil || s.wellKnown || !w.mark(unsafe.Pointer(s)) {
		return
	}
	w.size += int64(unsafe.Sizeof(Symbol{}))
	w.str(s.description)
	if s.weak != nil {
		w.weakSide(s.weak)
	}
}

// bigint counts a BigInt and its words.
func (w *liveWalker) bigint(b *BigInt) {
	if b == nil || !w.mark(unsafe.Pointer(b)) {
		return
	}
	w.size += int64(unsafe.Sizeof(BigInt{})) + int64(cap(b.v.Bits()))*bits.UintSize/8
}

// private adds a private name: its description and the methods and
// accessors of a class it carries.
func (w *liveWalker) private(p *PrivateName) {
	for ; p != nil && w.mark(unsafe.Pointer(p)); p = p.brand {
		w.size += int64(unsafe.Sizeof(PrivateName{}))
		w.str(p.desc)
		w.object(p.home)
		w.object(p.get)
		w.object(p.set)
	}
}

package engine

import (
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// Memory limit (docs/DESIGN.md §4.4).
//
// A realm with a memory limit charges what it allocates for JavaScript to
// an account behind r.lazy: objects by the size of their struct, the
// storage of their properties and elements as it grows, strings by their
// header and units, Map and Set tables, ArrayBuffers, environments,
// suspended generator frames, BigInt results, the code eval and the
// Function constructors compile, the wrappers FromGo and JSON.parse build,
// and the register stack and frame table as they grow (moveStack,
// growFrames). The count is approximate: a producer charges what it asks
// the Go allocator for, not the size class it gets, and the places that
// grow a slice without one of the charge points below are not counted.
// Nothing is credited back as it is freed: the account counts allocation
// since the last reset (ReleaseCallData, ResetAllocation) or measurement.
//
// A measurement of what the realm's heap retains (memlive.go), lastLive,
// replaces the allocation counted until then, so that the limit bounds
// what the program keeps plus what it allocated since the measurement. A
// host that keeps a program alive across requests or callbacks (an event
// loop) measures between them (RebaseAllocation); a reset zeroes the
// allocation and keeps the measurement, so what the program keeps across
// requests still counts.
//
// When lastLive plus the bytes allocated since the last reset or
// measurement pass the limit, the realm measures its heap (remeasure): the
// first time between two resets or rebases, with or without a measurement
// before, and again as RebaseAllocation's pacing allows. It does so only
// where no builtin may hold what it made where the walk cannot reach it:
// in interpreted code, and in the callbacks of the builtins that keep what
// they build on the register stack or report it (memlive.go, remeasure).
// While another builtin runs, or the host's ParseJSONString or FromGo, the
// charges stand within a slack past the limit (half of what it leaves,
// plus the one allocation that crosses that), and the interpreter measures
// once the native returns or at its next frame entry, as the drain does
// between promise jobs. The host's Go code outside any call, and its ChargeMemory
// where no script runs, measure at once, as RebaseAllocation does.
// The realm interrupts itself with a *MemoryLimitError when the
// measurement passes the limit too, or when no measurement may be made.
// Allocation cannot fail (initObject and initArray return no error), and
// an interrupt unwinds without running any catch or finally and drops the
// queued jobs, so a script cannot catch the hit and keep allocating. The
// producers that know a result's size before they allocate it, and throw
// a RangeError for a size past the engine's limits (String.prototype.repeat,
// new ArrayBuffer, a BigInt shift), throw the same RangeError for a size
// past what is left of the budget, which the script can catch.
//
// With no limit set r.lazy.mem is nil, and a charge point costs the loads
// of r.lazy and its mem field: the account, and every counter it keeps,
// exists only while a limit is set.

// ErrMemoryLimit is the error a *MemoryLimitError unwraps to, so that
// errors.Is finds it through the *InterruptedError a hit stops the realm
// with.
var ErrMemoryLimit = errors.New("memory limit exceeded")

// MemoryLimitError is the value a realm interrupts itself with when what
// it allocated since the last reset, plus what the last measurement found
// it retains (RebaseAllocation), passes its memory limit, and the error of
// a RebaseAllocation whose measurement passes it.
type MemoryLimitError struct {
	// Limit is the limit that was passed, in bytes.
	Limit int64
	// Allocated is what was charged since the last reset or rebase, the
	// allocation that passed the limit included; 0 for the error of a
	// RebaseAllocation.
	Allocated int64
	// Live is the retained size the last measurement of the realm's heap
	// found (RebaseAllocation, or a charge past the limit), which the limit
	// counts with Allocated; 0 while none was made.
	Live int64
	// Stack is the JavaScript stack at the allocation, the "    at ..."
	// lines of an Error's stack; "" when no JavaScript ran.
	Stack string
}

// Error implements error.
func (e *MemoryLimitError) Error() string {
	s := "memory limit exceeded: " + strconv.FormatInt(e.Live+e.Allocated, 10) + " bytes"
	if e.Live != 0 {
		s += " (" + strconv.FormatInt(e.Live, 10) + " retained, " + strconv.FormatInt(e.Allocated, 10) + " allocated since)"
	} else {
		s += " allocated"
	}
	return s + ", limit " + strconv.FormatInt(e.Limit, 10)
}

// Unwrap returns ErrMemoryLimit.
func (e *MemoryLimitError) Unwrap() error { return ErrMemoryLimit }

// Stats are counters of a realm, each read in constant time. The
// allocation counters (AllocatedBytes to MemoryLimitHits) count only while
// a memory limit is set: they are the limit's account, which a realm
// without a limit does not keep. A host that wants them without a limit
// sets one it never reaches (math.MaxInt64).
type Stats struct {
	// AllocatedBytes is what was charged since the limit was set.
	AllocatedBytes int64
	// RequestAllocatedBytes is what was charged since the last reset
	// (ReleaseCallData, ResetAllocation) or measurement, which the limit
	// bounds with LiveBytes.
	RequestAllocatedBytes int64
	// LiveBytes is the last measurement of what the realm retains
	// (RebaseAllocation, or a charge past the limit), which resets keep; 0
	// while none was made.
	LiveBytes int64
	// Objects, Strings and Shapes count the objects, strings and shapes
	// charged since the limit was set.
	Objects int64
	Strings int64
	Shapes  int64
	// MemoryLimitHits counts the times the limit stopped the realm, and
	// the measurements of RebaseAllocation that passed it.
	MemoryLimitHits int64
	// LastMemoryLimitError is the error of the last hit since the last
	// reset or rebase, or nil. The interrupt a hit raises is the realm's
	// single pending interrupt, which a concurrent Interrupt can replace (a
	// host's timeout): the hit is still counted, and its error kept, here.
	LastMemoryLimitError *MemoryLimitError
	// Interrupts counts the calls of Interrupt, memory limit hits included.
	Interrupts int64
	// ICEntries is the number of inline cache entries of the functions the
	// realm has run.
	ICEntries int
	// PendingJobs is the number of queued promise jobs and microtasks.
	PendingJobs int
	// RegisterStackBytes is the size of the interpreter's register stack.
	RegisterStackBytes int64
}

// memKind is what a charge counts besides its bytes.
type memKind uint8

const (
	memBytes memKind = iota
	memObject
	memString
	memShape
	memKinds
)

// memAccount is the account of a realm with a memory limit (r.lazy.mem).
// Only the goroutine running the realm uses it, but flat.
type memAccount struct {
	limit     int64
	allocated int64 // charged since the last reset or measurement
	// lastLive is the retained size the last measurement of the heap found
	// (RebaseAllocation, remeasure), 0 while none was made; the limit counts
	// it with allocated. A reset keeps it.
	lastLive int64
	// mark is total at the last measurement: total - mark is what was
	// charged since, resets included, which paces the next one (due).
	mark int64
	// room and dueAt follow from limit, lastLive and mark (pace), so that
	// a charge and the check of RebaseAllocation compare once: room is
	// what may be allocated before the limit, limit - lastLive, and dueAt
	// the total at which the next measurement is due.
	room, dueAt int64
	// remeasured tells that a charge past the limit measured since the
	// last RebaseAllocation or reset (remeasure), and measuring that a
	// measurement runs.
	remeasured, measuring bool
	// holding counts the builtins running that keep JavaScript values only
	// in Go while they call back into JavaScript (holdingBegin): while it is
	// not zero, a charge past the limit does not measure. held is the bytes
	// of Go memory the builtins running keep while they call back
	// (holdBytes), which a measurement adds to what it walks.
	holding int32
	held    int64
	// jsonText is the share root of the strings a parse in progress reads
	// out of its text (shareText); nil between parses. jsonVals and
	// jsonKeys are the capacities of the parse's value and key stacks last
	// seen (jsonParser.every), whose growth is charged.
	jsonText           *String
	jsonVals, jsonKeys int
	total              int64 // charged since the limit was set
	counts             [memKinds]int64
	hits               int64
	last               *MemoryLimitError // the last hit's, since the last reset or rebase
	hitting            bool              // memoryLimitHit runs: its own charges do not hit again
	// hostCharge tells that the charge being made is the host's
	// (ChargeMemory), which with no frame running measures at once
	// (builtinRuns).
	hostCharge bool
	// flat is what flattening this realm's ropes allocated since the last
	// charge: a rope flattens wherever it is first read, which may be on
	// another goroutine than the realm's (a value the host kept), so the
	// rope (ropeNode) adds here atomically and the next charge takes it
	// over.
	flat atomic.Int64
}

// Sizes charged for storage that grows.
const (
	// stringHeaderSize is what a String costs besides its units.
	stringHeaderSize = int(unsafe.Sizeof(String{}))
	// valueSize is a slot, an element or a register.
	valueSize = int(unsafe.Sizeof(Value{}))
	// sparseEntrySize is an element outside dense storage, a map entry.
	sparseEntrySize = int(unsafe.Sizeof(propCell{})) + 16
	// shapeSize is a shape and the record of its transition.
	shapeSize = int(unsafe.Sizeof(Shape{})) + 32
	// shapePropSize is an entry of a shape's props.
	shapePropSize = int(unsafe.Sizeof(shapeProp{}))
)

// appendValue appends v to s as append does, charging the storage when s
// grows.
func (r *Realm) appendValue(s []Value, v Value) []Value {
	if len(s) == cap(s) {
		s = r.growValues(s, 1)
	}
	return append(s, v)
}

// growValues returns s with room for n more values and charges the new
// storage. Up to growLargeMin values it grows as append does, doubling;
// past them append grows by a quarter and then less, so an array filled by
// push allocates about five times its final size in all, and the copies
// were 23 % of the bytes a server render of a React page allocated. There
// s keeps doubling: about twice the final size in all. Growing by half, as
// QuickJS grows its arrays, saved less, and from growLargeMin to about
// 1,500 values allocated more than append, which grows by more than half
// up to there.
//
// From half of growLargeMin, the storage of an array literal site's latest
// result may jump to the room the site's large results took instead
// (siteTable.grow).
func (r *Realm) growValues(s []Value, n int) []Value {
	if c := cap(s); c >= growLargeMin && len(s)+n > c {
		ns := make([]Value, len(s), max(len(s)+n, 2*c))
		copy(ns, s)
		s = ns
	} else if c >= siteBigMin && len(s)+n > c && r.lazy != nil && r.lazy.sites != nil {
		s = r.lazy.sites.grow(s, n)
	} else {
		s = slices.Grow(s, n)
	}
	r.charge(cap(s) * valueSize)
	return s
}

// growLargeMin is the capacity from which growValues doubles a slice where
// append grows it by less.
const growLargeMin = 256

// SetMemoryLimit sets the number of bytes the realm's JavaScript may
// retain, as the last measurement of its heap found, plus allocate since
// that measurement or the last reset (ReleaseCallData, ResetAllocation),
// before it interrupts itself with a *MemoryLimitError. A charge past the
// limit measures first (see the file comment), so a program that keeps
// little runs on however much it allocates. n <= 0 removes the limit and
// its counters. Changing a limit keeps what was counted.
func (r *Realm) SetMemoryLimit(n int64) {
	if n <= 0 {
		if r.lazy != nil {
			r.lazy.mem = nil
		}
		return
	}
	lz := r.lazyState()
	if lz.mem == nil {
		lz.mem = &memAccount{}
	}
	lz.mem.limit = n
	lz.mem.pace()
}

// pace computes room and dueAt after limit, lastLive or mark changed.
func (m *memAccount) pace() {
	m.room = m.limit - m.lastLive
	m.dueAt = m.mark + max((m.limit-m.lastLive)/2, m.lastLive/2)
}

// MemoryLimit returns the realm's memory limit, 0 for none.
func (r *Realm) MemoryLimit() int64 {
	if m := r.mem(); m != nil {
		return m.limit
	}
	return 0
}

// ResetAllocation starts a new budget: the bytes charged since the last
// reset go back to zero and the last hit's error is forgotten. The last
// measurement of what the realm retains (RebaseAllocation) stays: the
// limit goes on counting what the program keeps. A pending interrupt, the
// hit's included, stays pending (ClearInterrupt). ReleaseCallData resets
// too; a host that does not pool calls this where a request starts. It may
// be called from a host function, which gives the running script a new
// budget.
func (r *Realm) ResetAllocation() {
	m := r.mem()
	if m == nil {
		return
	}
	if f := m.flat.Swap(0); f != 0 {
		m.total += f
	}
	m.allocated = 0
	m.last = nil
	m.remeasured = false
}

// Stats returns the realm's counters. Like every method but Interrupt and
// ClearInterrupt, it runs on the goroutine using the realm, and it may be
// called from a host function.
func (r *Realm) Stats() Stats {
	s := Stats{
		ICEntries:          len(r.ic),
		RegisterStackBytes: int64(cap(r.interp.stack)) * int64(unsafe.Sizeof(Value{})),
	}
	if p := r.interruptValue.Load(); p != nil {
		s.Interrupts = p.n
	}
	lz := r.lazy
	if lz == nil {
		return s
	}
	if j := lz.jobs; j != nil {
		s.PendingJobs = len(j.queue) - j.head
	}
	if m := lz.mem; m != nil {
		f := m.flat.Load()
		s.AllocatedBytes = m.total + f
		s.RequestAllocatedBytes = m.allocated + f
		s.LiveBytes = m.lastLive
		s.Objects = m.counts[memObject]
		s.Strings = m.counts[memString]
		s.Shapes = m.counts[memShape]
		s.MemoryLimitHits = m.hits
		s.LastMemoryLimitError = m.last
	}
	return s
}

// mem returns the realm's memory account, nil while no limit is set.
func (r *Realm) mem() *memAccount {
	if lz := r.lazy; lz != nil {
		return lz.mem
	}
	return nil
}

// charge charges n bytes of storage to the realm's memory account. With no
// limit set it costs two loads and two branches; the account is updated
// out of line, so callers keep their inlining.
func (r *Realm) charge(n int) {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		r.chargeSlow(n, memBytes)
	}
}

// chargeObject charges an object of n bytes (its struct and what is
// allocated with it).
func (r *Realm) chargeObject(n uintptr) {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		r.chargeSlow(int(n), memObject)
	}
}

// chargeString charges a string whose units take n bytes besides its
// header, and the header.
func (r *Realm) chargeString(n int) {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		r.chargeSlow(stringHeaderSize+n, memString)
	}
}

// chargeGoString charges a string made from Go text, such as the message
// of an error the engine throws or an error's stack, which can hold a
// script's string: a byte per character of an ASCII string, two per UTF-16
// unit otherwise, and the header.
func (r *Realm) chargeGoString(s *String) {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		n := s.Len()
		if s.kind != strASCII {
			n *= 2
		}
		r.chargeSlow(stringHeaderSize+n, memString)
	}
}

// reserve charges n bytes its caller is about to allocate and returns the
// interrupt's error when the charge passed the limit, or a host
// interrupted, so that the caller stops before it allocates them.
func (r *Realm) reserve(n int) error {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		r.chargeSlow(n, memBytes)
		return r.CheckInterrupt()
	}
	return nil
}

// chargeShape charges a shape of the realm's own tree and its transition
// record.
func (r *Realm) chargeShape() {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		r.chargeSlow(shapeSize, memShape)
	}
}

// overBudget reports whether n more bytes would pass the memory limit:
// a producer that knows its size before it allocates throws its RangeError
// rather than allocate them (see the file comment).
func (r *Realm) overBudget(n int) bool {
	if lz := r.lazy; lz != nil && lz.mem != nil {
		return r.overBudgetSlow(n)
	}
	return false
}

//go:noinline
func (r *Realm) overBudgetSlow(n int) bool {
	m := r.lazy.mem
	if m.allocated+m.flat.Load()+int64(n) <= m.room {
		return false
	}
	switch r.remeasure(m) {
	case measuredNow:
		return m.allocated+int64(n) > m.room
	case measureWaits:
		r.interruptFlag.CompareAndSwap(0, flagMeasure)
		r.endRunSees()
		return m.allocated+m.flat.Load()+int64(n) > m.room+m.slack()
	}
	return true
}

// chargeSlow adds n bytes of kind to the account and interrupts the realm
// when they pass the limit.
//
//go:noinline
func (r *Realm) chargeSlow(size int, kind memKind) {
	m := r.lazy.mem
	n := int64(size)
	if m.flat.Load() != 0 {
		n += m.flat.Swap(0)
	}
	m.allocated += n
	m.total += n
	m.counts[kind]++
	if m.allocated > m.room && !r.overLimit(m, n) {
		r.memoryLimitHit(m)
	}
}

// overLimit handles a charge of n bytes that took the account past the
// limit and reports whether it stands: the measurement it made left room
// for it, or a builtin runs and the account is within the slack the
// measurement waits in (remeasure).
//
//go:noinline
func (r *Realm) overLimit(m *memAccount, n int64) bool {
	switch r.remeasure(m) {
	case measuredNow:
		// The charge is of what its caller is about to allocate.
		m.allocated, m.mark = n, m.total-n
		m.pace()
		return n <= m.room
	case measureWaits:
		// The interpreter measures at its next check of the flag, once the
		// builtin returned (pendingInterrupt).
		r.interruptFlag.CompareAndSwap(0, flagMeasure)
		r.endRunSees()
		return m.allocated <= m.room+m.slack()
	}
	return false
}

// chargeMany is chargeSlow for count objects or strings of size bytes in
// all.
func (r *Realm) chargeMany(size int, kind memKind, count int) {
	r.chargeSlow(size, kind)
	r.lazy.mem.counts[kind] += int64(count - 1)
}

// memoryLimitHit interrupts the realm with a *MemoryLimitError carrying
// the JavaScript stack. While an interrupt is pending, a host's or an
// earlier hit's, the realm is stopping already, so it does nothing; once
// the host clears it, the next charge past the limit hits again until a
// reset.
func (r *Realm) memoryLimitHit(m *memAccount) {
	if m.hitting || r.interrupted() {
		return
	}
	m.hitting = true
	e := &MemoryLimitError{Limit: m.limit, Allocated: m.allocated, Live: m.lastLive}
	// The interpreter stores each frame's pc before the instruction runs,
	// so the stack is exact at any allocation. Formatting reads data
	// properties only and runs no JavaScript.
	if frames := captureStackInto(r, nil, nil, defaultStackTraceLimit); len(frames) != 0 {
		e.Stack = formatStack(r, frames)
	}
	m.hitting = false
	m.hits++
	m.last = e
	r.Interrupt(e)
	r.endRunSees()
}

// shareText sets the share root of the strings a parse reads out of src
// (jsonParser.plain, Realm.lazyStringSized): text, whose units or lazy source src
// is, or a String of its own for the host's text, a working copy or a text
// FromGo carved (shareRoot), which keeps its bytes for the live measurement
// to count; its kind is strASCII whatever the bytes, and it is never a
// value.
func (m *memAccount) shareText(src string, text *String) {
	switch {
	case len(src) < shareMin:
		text = nil // values count their own units (shareRoot)
	case text == nil:
		text = &String{s: src, kind: strASCII}
	case !text.needsFlat():
		if text = text.shareRoot(); text == nil {
			text = &String{s: src, kind: strASCII}
		}
	}
	m.jsonText = text
}

// substring is s.Substring charging the String it makes: a substring of
// an ASCII string shares its bytes, one of a UTF-16 string copies its
// units.
func (r *Realm) substring(s *String, start, end int) *String {
	if lz := r.lazy; lz != nil && lz.mem != nil && start < end && (start > 0 || end < s.Len()) {
		// A rope's or lazy string's kind never changes: its contents are
		// read through flat, of whose result only the kind is read, so a
		// rope's view stays on the stack.
		f := s
		if s.needsFlat() {
			f = s.flat(new(String))
		}
		n := 0
		if f.kind == strUTF16 {
			n = 2 * (end - start)
		}
		r.chargeSlow(stringHeaderSize+n, memString)
	}
	return s.Substring(start, end)
}

// compiledSize estimates the memory of code compiled at run time and of
// the meta tree the realm keeps for it: its functions, instructions,
// constants, handlers and line tables.
func compiledSize(fn *bytecode.Function) int {
	n := int(unsafe.Sizeof(bytecode.Function{})) + int(unsafe.Sizeof(funcMeta{})) +
		4*len(fn.Code) + int(unsafe.Sizeof(bytecode.Const{}))*len(fn.Consts) +
		int(unsafe.Sizeof(bytecode.Handler{}))*len(fn.Handlers) + int(unsafe.Sizeof(bytecode.LineEntry{}))*len(fn.LineTable)
	for _, c := range fn.Children {
		n += compiledSize(c)
	}
	return n
}

// ropeNode is a rope of a realm with a memory limit: the String and the
// account its flattening charges, in one allocation (56 bytes, the 64-byte
// class) marked strCharged. A rope is flattened wherever it is first read,
// on any goroutine, where no realm is at hand. The flag and m are written
// once, before the rope is published, and never again, so two goroutines
// flattening one rope (a race of its own, on the String's fields) at worst
// both charge the right account.
//
// Undercounts accepted: a rope made before the realm had a limit is a plain
// String and its flattening is not charged; one made under an account that
// SetMemoryLimit(0) then dropped charges that account, which nothing reads.
type ropeNode struct {
	s String
	m *memAccount
}

// newChargedRope is Concat's rope of a and b, n units long, for a realm
// with an account: a ropeNode, charged its size.
func (r *Realm) newChargedRope(a, b *String, n int) *String {
	x := &ropeNode{
		s: String{p: unsafe.Pointer(a), right: b, n: int32(n), kind: strRope, flags: strCharged},
		m: r.lazy.mem,
	}
	r.chargeString(int(unsafe.Sizeof(ropeNode{})) - stringHeaderSize)
	return &x.s
}

// chargeFlatten charges the n bytes the flattening of the rope s allocates
// to the account of the realm that made it, if it had a limit (ropeNode).
// It may run on any goroutine.
func (s *String) chargeFlatten(n int) {
	if s.flags&strCharged != 0 {
		(*ropeNode)(unsafe.Pointer(s)).m.flat.Add(int64(n))
	}
}

// ChargeMemory charges n bytes a host function allocates for JavaScript,
// such as a buffer it hands to NewArrayBuffer, to the realm's memory limit
// (none: a no-op). It returns the *InterruptedError of a pending interrupt,
// which the charge raises when it passes the limit, so that the host
// function can return it before it allocates. Past the limit with no
// script running, from the host's own code (outside any call, or in one it
// holds with HoldJobs, as an event loop's task) or from a host function
// called from Go, the charge measures at once, as RebaseAllocation does:
// what the host holds there it registers (AddLiveRoots), and a host
// function keeps what it built where the walk finds it, as across its
// callbacks.
func (r *Realm) ChargeMemory(n int) error {
	if n <= 0 {
		return nil
	}
	if m := r.mem(); m != nil {
		m.hostCharge = true
		defer m.endHostCharge()
		r.chargeSlow(n, memBytes)
		return r.CheckInterrupt()
	}
	return nil
}

// endHostCharge ends ChargeMemory's charge, also one a root's panic ended.
func (m *memAccount) endHostCharge() { m.hostCharge = false }

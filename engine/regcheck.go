//go:build moejsregcheck

package engine

import (
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"sync"
	"unsafe"
	"weak"

	"github.com/MoeForge/moejs/bytecode"
)

// The checked build of the interpreter (go test -tags moejsregcheck): run
// is interp_run_regcheck.go, a copy of interp_run.go made by go generate
// (internal/gen/regcheck), whose registers are a *checkedRegs. Each access
// the fast run makes through its window without a bounds check goes
// through checkedRegs.at here instead, and panics unless
//
//   - the register is in the layout bytecode.RegisterSpans gives the
//     instruction being run, or is the register of the handler being
//     entered (a layout row that leaves out a register run touches, and an
//     opcode run runs without a row, fail here);
//   - it is below the frame's NumRegs, and the frame's registers are in
//     the stack (len(st.stack) >= base+NumRegs) when run takes its window;
//   - the stack has not moved since run last took its window (run keeps
//     no window across an operation that grows the stack).
//
// and dispatch panics on a pc that starts no instruction of the walk
// bytecode.VerifyRegisters makes: a jump, a handler, a resumption or a
// module body that lands inside an instruction would run words no one
// checked. It also panics when the instruction run before changed a
// register its layout names that bytecode.RegisterWrites leaves out, or
// completed and left a register of a run RegisterWrites gives a kind
// with another: the verifier's proof of the operand kinds run reads
// without a check (bytecode.KindOperands) takes the rows for what an
// instruction does, its cold paths included.
//
// Those are what the fast run relies on (window). The accesses themselves
// are ordinary slice indexes, so the checked build runs under checkptr and
// the race detector as any code does. The operations run hands to coldOp,
// iterOp and the class, generator, async and sloppy helpers index the
// stack through slices of the frame and Go's bounds checks in either build.

// regSpans is the layout the checked run compares its accesses with; a
// test replaces it to break a row.
var regSpans = bytecode.RegisterSpans

// regWrites is the write table the checked run compares its instructions
// with; a test replaces it to break a row.
var regWrites = bytecode.RegisterWrites

// regcheckObserve, when a test sets it, sees every instruction the checked
// run dispatches, with its first extra word.
var regcheckObserve func(op bytecode.Op, w, x uint32)

// checkedRegs stands for run's window in the checked build.
type checkedRegs struct {
	st     *interpState
	base   int
	n      int    // NumRegs
	fn     string // the function's name, for the panics
	starts []bool // the instruction starts of the function's code
	stack  *Value // the stack's array when run last took its window
	op     bytecode.Op
	pc     int          // the instruction being run; -1 while a handler is entered
	ok     [4]uint64    // the registers it may name
	writes *writeChecks // what each instruction of the function writes
	kept   []Value      // the registers of writes.kept at pc, as it found them
	buf    [4]Value     // kept's first array
}

func newCheckedRegs(st *interpState, base int, code *bytecode.Function) *checkedRegs {
	wc := writeChecksOf(code)
	c := &checkedRegs{st: st, base: base, n: int(code.NumRegs), fn: code.Name, starts: wc.starts, writes: wc, pc: -2}
	c.kept = c.buf[:0]
	return c.refresh()
}

// startsOf returns the instruction starts of code as
// bytecode.VerifyRegisters walks it: from pc 0, an instruction and its
// extra words at a time, a word that is no opcode alone.
func startsOf(code *bytecode.Function) []bool {
	s := make([]bool, len(code.Code))
	for pc := 0; pc < len(code.Code); {
		s[pc] = true
		op := bytecode.DecodeOp(code.Code[pc])
		if int(op) >= bytecode.OpCount {
			pc++
			continue
		}
		pc += 1 + op.ExtraWords()
	}
	return s
}

// writeChecks is what the checked run keeps of a function: its
// instruction starts (startsOf), and for each instruction the registers
// its layout names below NumRegs that its write row leaves out
// (kept[keptAt[pc]:keptAt[pc+1]]) and the runs its row gives a kind
// (claims[claimAt[pc]:claimAt[pc+1]]), from the tables it was made with.
type writeChecks struct {
	starts          []bool
	tables          [2]uintptr // regSpans and regWrites
	keptAt, claimAt []int32
	kept            []uint8
	claims          []writeClaim
	rowless         []bool // the pcs of instructions with no write row (nil: none)
}

// writeClaim is a run of registers a write row gives a kind.
type writeClaim struct {
	r, k uint8
	kind bytecode.RegKind
}

// instrWrites caches the writeChecks of each function the checked run ran
// with the real tables; its keys are weak, so a template no one
// references (a fuzzed one) drops out.
var instrWrites sync.Map // weak.Pointer[bytecode.Function] -> *writeChecks

var realTables = [2]uintptr{reflect.ValueOf(bytecode.RegisterSpans).Pointer(), reflect.ValueOf(bytecode.RegisterWrites).Pointer()}

// writeChecksOf returns the writeChecks of code, made once per function
// with the real tables and again for each run with a test's.
func writeChecksOf(code *bytecode.Function) *writeChecks {
	tables := [2]uintptr{reflect.ValueOf(regSpans).Pointer(), reflect.ValueOf(regWrites).Pointer()}
	k := weak.Make(code)
	if v, ok := instrWrites.Load(k); ok && v.(*writeChecks).tables == tables {
		return v.(*writeChecks)
	}
	n := len(code.Code)
	starts := startsOf(code)
	wc := &writeChecks{starts: starts, tables: tables, keptAt: make([]int32, n+1), claimAt: make([]int32, n+1)}
	for pc := 0; pc < n; pc++ {
		wc.keptAt[pc], wc.claimAt[pc] = int32(len(wc.kept)), int32(len(wc.claims))
		if !starts[pc] {
			continue
		}
		w := code.Code[pc]
		op := bytecode.DecodeOp(w)
		if int(op) >= bytecode.OpCount {
			continue
		}
		var x uint32
		if op.ExtraWords() > 0 && pc+1 < n {
			x = code.Code[pc+1]
		}
		var spans, writes [4]uint64
		regSpans(op, w, x, func(r, k int) {
			for i := max(r, 0); i < r+k && i < windowRegs; i++ {
				spans[i>>6] |= 1 << (i & 63)
			}
		})
		if !regWrites(op, w, x, func(r, k int, kind bytecode.RegKind) {
			for i := max(r, 0); i < r+k && i < windowRegs; i++ {
				writes[i>>6] |= 1 << (i & 63)
			}
			if kind != bytecode.RegAny && r >= 0 && r+k <= int(code.NumRegs) {
				wc.claims = append(wc.claims, writeClaim{uint8(r), uint8(k), kind})
			}
		}) {
			if wc.rowless == nil {
				wc.rowless = make([]bool, n)
			}
			wc.rowless[pc] = true
		}
		for i := range spans {
			for kept := spans[i] &^ writes[i]; kept != 0; kept &= kept - 1 {
				if r := i<<6 | bits.TrailingZeros64(kept); r < int(code.NumRegs) {
					wc.kept = append(wc.kept, uint8(r))
				}
			}
		}
	}
	wc.keptAt[n], wc.claimAt[n] = int32(len(wc.kept)), int32(len(wc.claims))
	if tables == realTables {
		if _, loaded := instrWrites.LoadOrStore(k, wc); !loaded {
			runtime.AddCleanup(code, func(k weak.Pointer[bytecode.Function]) { instrWrites.Delete(k) }, k)
		}
	}
	return wc
}

// refresh takes the window again, as run does after an operation that may
// move the stack.
func (c *checkedRegs) refresh() *checkedRegs {
	if len(c.st.stack) < c.base+c.n {
		panic(fmt.Sprintf("engine: regcheck: %q runs with registers %d..%d past the stack's %d", c.fn, c.base, c.base+c.n-1, len(c.st.stack)))
	}
	c.stack = unsafe.SliceData(c.st.stack)
	return c
}

// dispatch records the instruction at pc as the one being run, once the
// one before it has completed.
func (c *checkedRegs) dispatch(insns []uint32, pc int) {
	c.checkWrites(true)
	if pc >= len(c.starts) || !c.starts[pc] {
		panic(fmt.Sprintf("engine: regcheck: %q dispatches pc %d, which starts no instruction", c.fn, pc))
	}
	w := insns[pc]
	op := bytecode.DecodeOp(w)
	c.op, c.pc, c.ok = op, pc, [4]uint64{}
	if int(op) >= bytecode.OpCount {
		return // run faults on it and touches no register
	}
	var x uint32
	if op.ExtraWords() > 0 && pc+1 < len(insns) {
		x = insns[pc+1]
	}
	if regcheckObserve != nil {
		regcheckObserve(op, w, x)
	}
	if !regSpans(op, w, x, c.allow) {
		panic(fmt.Sprintf("engine: regcheck: no register layout for %s (%q pc %d)", op, c.fn, pc))
	}
	if c.writes.rowless != nil && c.writes.rowless[pc] {
		panic(fmt.Sprintf("engine: regcheck: no write row for %s (%q pc %d)", op, c.fn, pc))
	}
	c.kept = c.kept[:0]
	for _, r := range c.writes.kept[c.writes.keptAt[pc]:c.writes.keptAt[pc+1]] {
		c.kept = append(c.kept, c.st.stack[c.base+int(r)])
	}
}

// checkWrites checks what the instruction run last did to its registers
// against bytecode.RegisterWrites: it changed none the row leaves out,
// and when it completed (rather than threw) every register of a run the
// row gives a kind holds one of that kind.
func (c *checkedRegs) checkWrites(completed bool) {
	if c.pc < 0 || int(c.op) >= bytecode.OpCount {
		return
	}
	wc := c.writes
	for i, r := range wc.kept[wc.keptAt[c.pc]:wc.keptAt[c.pc+1]] {
		if c.st.stack[c.base+int(r)] != c.kept[i] {
			panic(fmt.Sprintf("engine: regcheck: %s at %q pc %d wrote register %d, which bytecode.RegisterWrites leaves out", c.op, c.fn, c.pc, r))
		}
	}
	if !completed {
		return
	}
	for _, cl := range wc.claims[wc.claimAt[c.pc]:wc.claimAt[c.pc+1]] {
		for i := int(cl.r); i < int(cl.r)+int(cl.k); i++ {
			if !hasRegKind(c.st.stack[c.base+i], cl.kind) {
				panic(fmt.Sprintf("engine: regcheck: %s at %q pc %d left in register %d a value that is not %s, which bytecode.RegisterWrites gives it", c.op, c.fn, c.pc, i, cl.kind))
			}
		}
	}
}

// hasRegKind reports whether v is of kind.
func hasRegKind(v Value, kind bytecode.RegKind) bool {
	switch kind {
	case bytecode.RegObject:
		return v.IsObject()
	case bytecode.RegArray:
		if !v.IsObject() {
			return false
		}
		_, ok := v.AsObject().internal.(*ArrayData)
		return ok
	case bytecode.RegIndex:
		f := v.AsNumber()
		return v.IsNumber() && f >= 0 && f == math.Trunc(f)
	}
	return true
}

// handler records the handler h as being entered, once the instruction
// that threw has: it writes its register, and the next one for any row
// but a catch.
func (c *checkedRegs) handler(h *bytecode.Handler) {
	c.checkWrites(false)
	c.pc, c.ok = -1, [4]uint64{}
	k := 2 // as run: the completion kind and the value for any row but a catch
	if h.Kind == bytecode.HandlerCatch {
		k = 1
	}
	c.allow(int(h.Reg), k)
}

func (c *checkedRegs) allow(r, k int) {
	for i := max(r, 0); i < r+k && i < windowRegs; i++ {
		c.ok[i>>6] |= 1 << (i & 63)
	}
}

// at is &regs[i] of the fast run, checked.
func (c *checkedRegs) at(i int) *Value {
	if unsafe.SliceData(c.st.stack) != c.stack {
		panic(fmt.Sprintf("engine: regcheck: %s at %q pc %d uses a window taken before the stack moved", c.op, c.fn, c.pc))
	}
	if i < 0 || i >= c.n {
		panic(fmt.Sprintf("engine: regcheck: %s at %q pc %d names register %d of %d", c.op, c.fn, c.pc, i, c.n))
	}
	if c.ok[i>>6]&(1<<(i&63)) == 0 {
		what := c.op.String()
		if c.pc == -1 {
			what = "a handler"
		}
		panic(fmt.Sprintf("engine: regcheck: %s at %q pc %d touches register %d, which its register layout leaves out", what, c.fn, c.pc, i))
	}
	return &c.st.stack[c.base+i]
}

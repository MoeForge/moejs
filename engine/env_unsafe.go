package engine

import "unsafe"

// Env is a closure environment: captured bindings of one scope. An Env is
// the first Value of a block of Values that holds its slots after it, so a
// scope costs one allocation and a 16-byte header: parent sits in that
// Value's pointer word and the slot count in its scalar word, which keeps
// the block's pointer map right for the collector.
type Env struct {
	parent *Env
	n      int
}

// The header fits in one Value with parent in its pointer word.
var (
	_ [unsafe.Sizeof(Value{}) - unsafe.Sizeof(Env{})]struct{}
	_ [0]struct{} = [unsafe.Offsetof(Value{}.ptr) + unsafe.Offsetof(Env{}.parent)]struct{}{}
)

// slots returns the slot storage that follows e in its block. A nil e
// panics as a nil dereference on its read of n, which comes before any
// slot is reached. checkptr (on under -race) would instead stop the
// program on the address after a nil e, so slots is exempt from it: a
// //go:nocheckptr function is not inlined when checkptr is on, so only
// checkptr builds pay for the call, and builds without it inline it.
//
//go:nocheckptr
func (e *Env) slots() []Value {
	h := sliceHeader{(*Value)(unsafe.Add(unsafe.Pointer(e), unsafe.Sizeof(Value{}))), e.n, e.n}
	return *(*[]Value)(unsafe.Pointer(&h))
}

// newEnvBlock allocates an environment of n slots below parent, the slots
// zero. Blocks up to five Values have their own allocation sites so that
// the common sizes skip makeslice; a block always has a Value after the
// header, so the slots' address stays inside it when n is 0.
func newEnvBlock(parent *Env, n int) *Env {
	var b *Value
	switch {
	case n <= 1:
		b = &new([2]Value)[0]
	case n == 2:
		b = &new([3]Value)[0]
	case n == 3:
		b = &new([4]Value)[0]
	case n == 4:
		b = &new([5]Value)[0]
	default:
		b = &make([]Value, n+1)[0]
	}
	e := (*Env)(unsafe.Pointer(b))
	e.parent, e.n = parent, n
	return e
}

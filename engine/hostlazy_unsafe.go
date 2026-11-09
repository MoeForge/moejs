package engine

import "unsafe"

// The object sentinel is the first field of hostHeap (its slot is 0) and the
// array sentinel the first of hostArraySentinel (slot 1), so a placeholder's
// shape pointer leads to its heap.
var (
	_ [0]struct{} = [unsafe.Offsetof(hostHeap{}.sentinel)]struct{}{}
	_ [0]struct{} = [unsafe.Offsetof(hostArraySentinel{}.shape)]struct{}{}
)

// hostHeapOf returns the heap whose sentinel s is; s must be a host sentinel.
func hostHeapOf(s *Shape) *hostHeap {
	if s.slot == 0 {
		return (*hostHeap)(unsafe.Pointer(s))
	}
	return (*hostArraySentinel)(unsafe.Pointer(s)).h
}

// boxedSlice is the data word of a Go slice held in an interface: the
// address of its boxed header. With the slice's type (a hostSlice* kind) it
// is the interface again, in half the space.
type boxedSlice struct{ p unsafe.Pointer }

func boxedSliceOf(v any) boxedSlice { return boxedSlice{(*[2]unsafe.Pointer)(unsafe.Pointer(&v))[1]} }

// slots returns an empty slots slice whose data pointer is b, the form in
// which an array node keeps its Go slice. Nothing reads through the pointer
// of an empty slice, and the collector keeps the boxed header alive through
// it like any other pointer to the start of an allocation.
func (b boxedSlice) slots() []Value { return unsafe.Slice((*Value)(b.p), 0) }

// boxedSliceIn returns the boxed slice an array node's slots hold. Slots
// JavaScript gave a named property have capacity, and ones it emptied again
// by turning the array into a dictionary are nil; neither holds one.
func boxedSliceIn(slots []Value) (boxedSlice, bool) {
	p := unsafe.Pointer(unsafe.SliceData(slots))
	return boxedSlice{p}, p != nil && cap(slots) == 0
}

// hostSliceTypes are the type words of the host slice kinds.
var hostSliceTypes = [...]unsafe.Pointer{
	hostSliceAny:    typeWord([]any(nil)),
	hostSliceString: typeWord([]string(nil)),
	hostSliceMaps:   typeWord([]map[string]any(nil)),
	hostSliceRaw:    typeWord(rawText("")),
}

func typeWord(v any) unsafe.Pointer { return (*[2]unsafe.Pointer)(unsafe.Pointer(&v))[0] }

// slice returns the interface b was taken from; kind is its hostSlice* type.
func (b boxedSlice) slice(kind uint8) (v any) {
	e := (*[2]unsafe.Pointer)(unsafe.Pointer(&v))
	e[0], e[1] = hostSliceTypes[kind], b.p
	return v
}

// sameEface reports whether a and b hold the same dynamic type and data word:
// the same map, or the same boxed slice header.
func sameEface(a, b any) bool {
	pa := (*[2]unsafe.Pointer)(unsafe.Pointer(&a))
	pb := (*[2]unsafe.Pointer)(unsafe.Pointer(&b))
	return pa[0] == pb[0] && pa[1] == pb[1]
}

// sameGoString reports whether a and b are the same Go string: the same
// length at the same address.
func sameGoString(a, b string) bool {
	return len(a) == len(b) && unsafe.StringData(a) == unsafe.StringData(b)
}

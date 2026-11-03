package engine

import "unsafe"

// A thenPromise and a hostPromise start with their promise, the promise's
// object first, so a pointer to the one is a pointer to the other.
var _ [0]struct{} = [unsafe.Offsetof(thenPromise{}.promiseObject) + unsafe.Offsetof(hostPromise{}.promiseObject) + unsafe.Offsetof(promiseObject{}.obj)]struct{}{}

// hostPromiseOfData returns the hostPromise whose promise's data d is; d
// must be marked host.
func hostPromiseOfData(d *promiseData) *hostPromise {
	return (*hostPromise)(unsafe.Add(unsafe.Pointer(d), -int(unsafe.Offsetof(promiseObject{}.data))))
}

// thenPromiseOfData returns the thenPromise whose promise's data d is; d
// must be marked derived.
func thenPromiseOfData(d *promiseData) *thenPromise {
	return (*thenPromise)(unsafe.Add(unsafe.Pointer(d), -int(unsafe.Offsetof(promiseObject{}.data))))
}

// asyncFrameWith co-allocates an async function call's state with the
// registers of its frame, an array of Values A.
type asyncFrameWith[A any] struct {
	af   asyncFrame
	regs A
}

// newAsyncFrameWith allocates the state with the array A of m registers,
// of which the frame's window uses n, and returns it with its size.
func newAsyncFrameWith[A any](n, m int) (*asyncFrame, uintptr) {
	x := &asyncFrameWith[A]{}
	x.af.g.regs = unsafe.Slice((*Value)(unsafe.Pointer(&x.regs)), m)[:n]
	return &x.af, unsafe.Sizeof(*x)
}

// allocAsyncFrame allocates the state of an async function call whose
// frame has n registers, with the registers when they fit one of the
// arrays below: their counts fill the allocator's size classes with the
// state. A larger frame's registers are an allocation of their own.
func allocAsyncFrame(n int) (*asyncFrame, uintptr) {
	switch {
	case n <= 4:
		return newAsyncFrameWith[[4]Value](n, 4)
	case n <= 6:
		return newAsyncFrameWith[[6]Value](n, 6)
	case n <= 8:
		return newAsyncFrameWith[[8]Value](n, 8)
	case n <= 10:
		return newAsyncFrameWith[[10]Value](n, 10)
	case n <= 12:
		return newAsyncFrameWith[[12]Value](n, 12)
	case n <= 14:
		return newAsyncFrameWith[[14]Value](n, 14)
	case n <= 16:
		return newAsyncFrameWith[[16]Value](n, 16)
	case n <= 18:
		return newAsyncFrameWith[[18]Value](n, 18)
	case n <= 20:
		return newAsyncFrameWith[[20]Value](n, 20)
	case n <= 22:
		return newAsyncFrameWith[[22]Value](n, 22)
	case n <= 26:
		return newAsyncFrameWith[[26]Value](n, 26)
	case n <= 30:
		return newAsyncFrameWith[[30]Value](n, 30)
	case n <= 34:
		return newAsyncFrameWith[[34]Value](n, 34)
	case n <= 38:
		return newAsyncFrameWith[[38]Value](n, 38)
	case n <= 46:
		return newAsyncFrameWith[[46]Value](n, 46)
	case n <= 54:
		return newAsyncFrameWith[[54]Value](n, 54)
	case n <= 62:
		return newAsyncFrameWith[[62]Value](n, 62)
	}
	af := &asyncFrame{}
	af.g.regs = make([]Value, n)
	return af, unsafe.Sizeof(asyncFrame{}) + uintptr(n*valueSize)
}

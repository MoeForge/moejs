package engine

import "unsafe"

// TypedArrayElements returns the elements of a typed array as a Go slice of
// its element type: []int8, []uint8 (a Uint8Array or Uint8ClampedArray),
// []int16, []uint16 (also a Float16Array, as its IEEE 754 binary16 bits),
// []int32, []uint32, []float32, []float64, []int64 or []uint64; ok is false
// for any other object. Like BufferData, the slice is the buffer's memory,
// not a copy, and is nil when the array is out of bounds. The elements are
// in little-endian byte order, which the slice reads correctly on a
// little-endian host only. ok is also false when the elements are not
// aligned for their type, which only a buffer over host bytes
// (NewArrayBuffer) that do not start at a word boundary can give: the data
// the engine allocates always is aligned (newBytes), and a view's offset is
// a multiple of its element size.
func (o *Object) TypedArrayElements() (elems any, ok bool) {
	ta, ok := dataOk(o, kindTypedArray)
	if !ok {
		return nil, false
	}
	b := ta.viewed()
	n := len(b) >> elemShift[ta.kind]
	p := unsafe.Pointer(unsafe.SliceData(b)) // nil when out of bounds
	if uintptr(p)&uintptr(elemSize[ta.kind]-1) != 0 {
		return nil, false
	}
	switch ta.kind {
	case elemInt8:
		return unsafe.Slice((*int8)(p), n), true
	case elemUint8, elemUint8Clamped:
		return unsafe.Slice((*uint8)(p), n), true
	case elemInt16:
		return unsafe.Slice((*int16)(p), n), true
	case elemUint16, elemFloat16:
		return unsafe.Slice((*uint16)(p), n), true
	case elemInt32:
		return unsafe.Slice((*int32)(p), n), true
	case elemUint32:
		return unsafe.Slice((*uint32)(p), n), true
	case elemFloat32:
		return unsafe.Slice((*float32)(p), n), true
	case elemFloat64:
		return unsafe.Slice((*float64)(p), n), true
	case elemBigInt64:
		return unsafe.Slice((*int64)(p), n), true
	}
	return unsafe.Slice((*uint64)(p), n), true
}

package engine

import "unsafe"

// asciiBytes returns a read-only []byte view of an ASCII string so that the
// regexp engine, and the base64 and hex decoders, can run on the string's
// own bytes (byte index == UTF-16 code unit index) without copying.
//
// GC safety: unsafe.Slice(unsafe.StringData(s), len(s)) points at the
// string's backing array, which stays reachable through the slice; the GC
// sees an ordinary pointer. The view is never written to: it is only passed
// to regexp.Regexp matching methods and to fromBase64 and fromHex, which
// read their input and copy out whatever they return. No pointer
// arithmetic is performed.
func asciiBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// unitBytes returns a read-only view of the bytes of UTF-16 code units, in
// the machine's byte order, for the required-literal scan (indexUnits),
// which looks for one byte of a unit with bytes.IndexByte.
//
// GC safety: the view aliases u's backing array, which stays reachable
// through it, and covers exactly its 2*len(u) bytes; it is only read and
// never outlives the scan.
func unitBytes(u []uint16) []byte {
	if len(u) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(u))), 2*len(u))
}

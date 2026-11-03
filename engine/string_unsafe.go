package engine

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"
)

// isASCII reports whether every byte of s is < 0x80, scanning 32 bytes at a
// time (SWAR, four words or-ed together), then eight, then one: every
// FromGoString of text scans it once.
//
// GC safety: unsafe.Slice(unsafe.StringData(s), len(s)) produces a []byte
// view over the string's own backing memory without copying. The view never
// outlives s (it is local to this function), it is never written to, and no
// pointer arithmetic is performed; the GC sees an ordinary pointer to the
// string's data, so this is safe. The view is read-only by construction.
func isASCII(s string) bool {
	if len(s) == 0 {
		return true
	}
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	for len(b) >= 32 {
		x := binary.LittleEndian.Uint64(b) | binary.LittleEndian.Uint64(b[8:]) |
			binary.LittleEndian.Uint64(b[16:]) | binary.LittleEndian.Uint64(b[24:])
		if x&swarMSB != 0 {
			return false
		}
		b = b[32:]
	}
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:])&swarMSB != 0 {
			return false
		}
	}
	for ; i < len(b); i++ {
		if b[i] >= 0x80 {
			return false
		}
	}
	return true
}

// bytesToString converts a byte slice to a string without copying. The
// caller must guarantee that b is never modified afterwards (StringBuilder
// only calls it on a buffer it is about to drop).
//
// GC safety: unsafe.String(&b[0], len(b)) yields a string header pointing at
// b's backing array; the array stays reachable through the string, and the
// caller relinquishes its own reference, so there is no aliasing of mutable
// data with an immutable string.
func bytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// Small ASCII strings are co-allocated with their header: the String and
// its bytes live in one allocation, so a short concatenation, case
// conversion or builder result costs one allocation instead of two.
// smallASCIIMax is the longest content that qualifies (a 64-byte payload
// puts the combined object in the 112-byte size class; longer payloads keep
// their own allocation). There is a buffer for each size class the header
// and up to 64 bytes fill (64, 80, 96 and 112 bytes), so that a string
// takes the class of its length.
const smallASCIIMax = 64

type asciiBuf16 struct {
	s String
	b [16]byte
}

type asciiBuf32 struct {
	s String
	b [32]byte
}

type asciiBuf48 struct {
	s String
	b [48]byte
}

type asciiBuf64 struct {
	s String
	b [smallASCIIMax]byte
}

// newASCIIBuf returns an ASCII String of length n together with the byte
// slice that backs it. The caller fills every byte before the string is
// published (returned, stored, or read) and never writes to it afterwards;
// until then the string is private to the caller, so there is no aliasing
// of mutable data with a visible immutable string. For n > smallASCIIMax
// the bytes are a separate allocation.
//
// GC safety: for small sizes the string's data pointer is an interior
// pointer into the same allocation as the header; the collector keeps the
// whole object alive through either pointer and no pointer arithmetic is
// performed. The layout invariant "s.s aliases b" is never observable: the
// String is immutable once published.
func newASCIIBuf(n int) (*String, []byte) {
	if n == 0 {
		return emptyString, nil
	}
	var (
		s *String
		b []byte
	)
	switch {
	case n <= 16:
		x := &asciiBuf16{}
		s, b = &x.s, x.b[:n:n]
	case n <= 32:
		x := &asciiBuf32{}
		s, b = &x.s, x.b[:n:n]
	case n <= 48:
		x := &asciiBuf48{}
		s, b = &x.s, x.b[:n:n]
	case n <= smallASCIIMax:
		x := &asciiBuf64{}
		s, b = &x.s, x.b[:n:n]
	default:
		s, b = &String{}, make([]byte, n)
	}
	*s = String{s: unsafe.String(unsafe.SliceData(b), n), n: int32(n), kind: strASCII}
	return s, b
}

// asciiStringCopy returns an ASCII String holding a copy of b (which the
// caller keeps ownership of); short contents are co-allocated with the
// header.
func asciiStringCopy(b []byte) *String {
	s, dst := newASCIIBuf(len(b))
	copy(dst, b)
	return s
}

// utf16Bytes returns a read-only []byte view of u's code units (native byte
// order), for hashing.
//
// GC safety: the view aliases u's backing array without copying; it is only
// passed to a hash function that reads it and never outlives the call, and
// no pointer arithmetic is performed beyond the slice header.
func utf16Bytes(u []uint16) []byte {
	if len(u) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(u))), 2*len(u))
}

// units returns the code units of a strUTF16 string. Their capacity is
// their length, so appending to them copies.
//
// GC safety: p is an unsafe.Pointer field, which the collector traces like
// any pointer; for a strUTF16 string it holds the address of the first of
// n code units in one allocation (unitsPtr), so the slice covers memory
// that p keeps alive.
func (s *String) units() []uint16 { return unsafe.Slice((*uint16)(s.p), s.n) }

// The words of a rope that flattening writes (flatten), which are only
// ever read and written with atomic operations: the children, p and right,
// which it releases, and the data pointer and length of s, which a rope
// does not use otherwise, in which it publishes the contents.
//
// GC safety: the data pointer of a string is traced like any pointer, and
// a rope stores in it the address of its contents, bytes or units, which
// keeps them alive. s stays a valid Go string: "" or, for ASCII contents,
// their bytes.
func (s *String) leftWord() *unsafe.Pointer  { return &s.p }
func (s *String) rightWord() *unsafe.Pointer { return (*unsafe.Pointer)(unsafe.Pointer(&s.right)) }
func (s *String) flatData() *unsafe.Pointer  { return (*unsafe.Pointer)(unsafe.Pointer(&s.s)) }
func (s *String) flatLen() *uintptr {
	return (*uintptr)(unsafe.Add(unsafe.Pointer(&s.s), unsafe.Sizeof(uintptr(0))))
}

// published returns the flat string of the contents a flattening of the
// rope s published, and false before one did: s.n bytes when the length
// word of s holds s.n, units when it holds 0.
func (s *String) published() (String, bool) {
	d := atomic.LoadPointer(s.flatData())
	if d == nil {
		return String{}, false
	}
	if atomic.LoadUintptr(s.flatLen()) != 0 {
		return String{s: unsafe.String((*byte)(d), s.n), n: s.n, kind: strASCII}, true
	}
	return String{p: d, n: s.n, kind: strUTF16}, true
}

// bytesString returns the bytes of b, which the caller never writes again,
// as a string without copying them.
func bytesString(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// unitsPtr is what a strUTF16 String keeps of u in p: the address of its
// first unit. The String covers u[:len(u)] only.
func unitsPtr(u []uint16) unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(u)) }

// utf16String wraps code units of which one at least is >= 0x80. It keeps
// u, which the caller must not modify afterwards.
func utf16String(u []uint16) *String {
	return &String{p: unitsPtr(u), n: int32(len(u)), kind: strUTF16}
}

type utf16Buf8 struct {
	s String
	u [8]uint16
}

type utf16Buf16 struct {
	s String
	u [16]uint16
}

type utf16Buf24 struct {
	s String
	u [24]uint16
}

type utf16Buf32 struct {
	s String
	u [32]uint16
}

type utf16Buf64 struct {
	s String
	u [64]uint16
}

// utf16BufMax is the most code units newUTF16Buf allocates with their
// String header.
const utf16BufMax = 64

// newUTF16Buf returns a UTF-16 String of n code units and the units, which
// its caller fills before publishing s, at least one of them >= 0x80. Up
// to utf16BufMax units share one typed allocation with their header, which
// the units' interior pointer keeps alive. Capacity is the logical length,
// as with newASCIIBuf, excluding size-class padding from the engine's
// string accounting. Lazy decoding and Intl formatting both use it.
func newUTF16Buf(n int) (*String, []uint16) {
	if n == 0 {
		return emptyString, nil
	}
	var s *String
	var u []uint16
	switch {
	case n <= 8:
		x := &utf16Buf8{}
		s, u = &x.s, x.u[:n:n]
	case n <= 16:
		x := &utf16Buf16{}
		s, u = &x.s, x.u[:n:n]
	case n <= 24:
		x := &utf16Buf24{}
		s, u = &x.s, x.u[:n:n]
	case n <= 32:
		x := &utf16Buf32{}
		s, u = &x.s, x.u[:n:n]
	case n <= utf16BufMax:
		x := &utf16Buf64{}
		s, u = &x.s, x.u[:n:n]
	default:
		s, u = &String{}, make([]uint16, n)
	}
	*s = String{p: unitsPtr(u), n: int32(n), kind: strUTF16}
	return s, u
}

// appendBuf heads an append buffer (string_append.go); its code units, ASCII
// bytes or UTF-16 units, follow it directly.
type appendBuf struct {
	used atomic.Int32 // units written; only the string this long writes past them
	cap  int32        // units the buffer holds
}

const appendBufSize = int(unsafe.Sizeof(appendBuf{}))

// appendBuf returns the buffer an inBuf string's units start.
//
// GC safety: those units start appendBufSize bytes into the allocation
// that holds the buffer (newAppendBuf), so stepping back from them stays
// inside that allocation, which the string's own pointer keeps alive.
func (s *String) appendBuf() *appendBuf {
	d := s.p
	if s.kind == strASCII {
		d = unsafe.Pointer(unsafe.StringData(s.s))
	}
	return (*appendBuf)(unsafe.Add(d, -appendBufSize))
}

func (h *appendBuf) data() unsafe.Pointer { return unsafe.Add(unsafe.Pointer(h), appendBufSize) }

// bytes and units return the whole buffer. Only the caller that moved used
// past a range writes it, before any string covers it.
func (h *appendBuf) bytes() []byte   { return unsafe.Slice((*byte)(h.data()), h.cap) }
func (h *appendBuf) units() []uint16 { return unsafe.Slice((*uint16)(h.data()), h.cap) }

// ascii returns the first n bytes of an ASCII buffer as a Go string. They
// are written once and for all, before any string covers them.
func (h *appendBuf) ascii(n int) string { return unsafe.String((*byte)(h.data()), n) }

// strBlock is a String allocated with an append buffer of D's size, the
// buffer its units start. Only the String has pointers: the collector
// scans its first 32 bytes, not the units.
type strBlock[D any] struct {
	s   String
	buf appendBuf
	d   D
}

func allocStrBlock[D any]() (*String, *appendBuf) {
	x := new(strBlock[D])
	return &x.s, &x.buf
}

// strBlocks are the block sizes, about 1.5x apart, each filling the size
// class in the comment (above 512 bytes an object with pointers holds an
// 8-byte type header besides: 56 + bytes + 8).
var strBlocks = [...]struct {
	bytes int
	alloc func() (*String, *appendBuf)
}{
	{72, allocStrBlock[[72]byte]},                         // 128
	{136, allocStrBlock[[136]byte]},                       // 192
	{200, allocStrBlock[[200]byte]},                       // 256
	{328, allocStrBlock[[328]byte]},                       // 384
	{456, allocStrBlock[[456]byte]},                       // 512
	{704, allocStrBlock[[704]byte]},                       // 768
	{960, allocStrBlock[[960]byte]},                       // 1024
	{1472, allocStrBlock[[1472]byte]},                     // 1536
	{1984, allocStrBlock[[1984]byte]},                     // 2048
	{3008, allocStrBlock[[3008]byte]},                     // 3072
	{4032, allocStrBlock[[4032]byte]},                     // 4096
	{6080, allocStrBlock[[6080]byte]},                     // 6144
	{8128, allocStrBlock[[8128]byte]},                     // 8192
	{12224, allocStrBlock[[12224]byte]},                   // 12288
	{16320, allocStrBlock[[16320]byte]},                   // 16384
	{24512, allocStrBlock[[24512]byte]},                   // 24576
	{appendBlockMax, allocStrBlock[[appendBlockMax]byte]}, // 32768
}

// appendBlockMax is the largest buffer allocated with its String; a larger
// one is an allocation of its own, without pointers.
const appendBlockMax = 32704

// newAppendBuf allocates an append buffer for n units of w bytes, used, and
// up to capacity, and the String that will head it, which the caller fills
// in: the largest block no longer than capacity that holds n, else the
// smallest that does, else a buffer of exactly capacity.
//
// GC safety: a buffer past appendBlockMax is a []byte, and appendBuf has
// no pointers, so viewing its first bytes as one hides nothing from the
// collector; the allocation is at least 8-byte aligned. The String keeps it
// alive through its pointer to the units.
func newAppendBuf(n, capacity, w int) (*String, *appendBuf) {
	capacity = min(capacity, MaxStringLength) // capacity*w fits an int32
	var (
		s *String
		h *appendBuf
	)
	if n*w <= appendBlockMax {
		i := 0
		for i+1 < len(strBlocks) && strBlocks[i+1].bytes <= capacity*w {
			i++
		}
		for strBlocks[i].bytes < n*w {
			i++
		}
		s, h = strBlocks[i].alloc()
		capacity = strBlocks[i].bytes / w
	} else {
		b := make([]byte, appendBufSize+capacity*w)
		s, h = new(String), (*appendBuf)(unsafe.Pointer(unsafe.SliceData(b)))
	}
	h.cap = int32(capacity)
	h.used.Store(int32(n))
	return s, h
}

// newAppendASCII returns an inBuf ASCII String of n bytes heading a new
// buffer of at least capacity, and the bytes, which the caller fills before
// publishing the String.
func newAppendASCII(n, capacity int) (*String, []byte) {
	s, h := newAppendBuf(n, capacity, 1)
	*s = String{s: h.ascii(n), n: int32(n), kind: strASCII, flags: strInBuf}
	return s, h.bytes()[:n]
}

// newAppendUTF16 is newAppendASCII for UTF-16 units.
func newAppendUTF16(n, capacity int) (*String, []uint16) {
	s, h := newAppendBuf(n, capacity, 2)
	*s = String{p: h.data(), n: int32(n), kind: strUTF16, flags: strInBuf}
	return s, h.units()[:n]
}

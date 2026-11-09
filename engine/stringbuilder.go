package engine

import (
	"slices"
	"unicode/utf16"
)

// StringBuilder accumulates code units and produces a *String. It stores
// ASCII bytes until the first unit >= 0x80 is written, then upgrades to
// UTF-16 storage once.
//
// The first smallASCIIMax ASCII bytes are written into an array inside the
// builder itself, addressed by index and never through a slice, so a
// builder declared as a local variable stays on the stack (nothing points
// into it), builds a short result without touching the heap, and String
// then hands out one co-allocated string. Longer results spill into a heap
// slice that the result aliases, as before.
type StringBuilder struct {
	b []byte   // heap ASCII storage once the inline array overflowed; nil before
	u []uint16 // UTF-16 storage after the upgrade; nil before
	n int32    // bytes used in inline while b == nil && u == nil
	// checked is the length up to which checkLength has nothing to do: the
	// capacity in code units it last saw (and charged to a memory limit,
	// memlimit.go), at most maxStringLength. It shares n's word, so that
	// the JSON stringifier holding a builder keeps its size class.
	checked int32
	inline  [smallASCIIMax]byte
}

// heldBytes is the Go memory the builder's storage takes, which a builtin
// reports while it calls back into JavaScript (holdBytes).
func (b *StringBuilder) heldBytes() int { return cap(b.b) + 2*cap(b.u) }

// Grow reserves capacity for n more code units.
func (sb *StringBuilder) Grow(n int) {
	if sb.u != nil {
		if cap(sb.u)-len(sb.u) < n {
			nu := make([]uint16, len(sb.u), 2*cap(sb.u)+n)
			copy(nu, sb.u)
			sb.u = nu
		}
		return
	}
	if sb.b == nil {
		if int(sb.n)+n > len(sb.inline) {
			sb.spill(int(sb.n) + n)
		}
		return
	}
	if cap(sb.b)-len(sb.b) < n {
		nb := make([]byte, len(sb.b), 2*cap(sb.b)+n)
		copy(nb, sb.b)
		sb.b = nb
	}
}

// spill moves the inline bytes to a heap slice of capacity c.
func (sb *StringBuilder) spill(c int) {
	nb := make([]byte, sb.n, c)
	copy(nb, sb.inline[:sb.n])
	sb.b = nb
	sb.n = 0
}

// growFor reserves n code units in the storage kind of s, so that copying
// from a UTF-16 string never goes through the ASCII buffer and an upgrade.
func (sb *StringBuilder) growFor(s *String, n int) {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strUTF16 {
		sb.growUnits(n)
		return
	}
	sb.Grow(n)
}

// growUnits reserves n code units for a result known to hold UTF-16: an
// empty builder starts in UTF-16 storage of that capacity.
func (sb *StringBuilder) growUnits(n int) {
	if sb.u == nil && sb.b == nil && sb.n == 0 {
		sb.u = make([]uint16, 0, n)
		return
	}
	sb.Grow(n)
}

// checkLength enforces the string length limit on a builder: producers
// whose output can outgrow their inputs (join, replace, JSON.stringify, case
// conversion) call it after each unbounded append, so the limit is raised
// as RangeError: Invalid string length before String would build a *String
// whose int32 length could not hold the result. It also charges the
// builder's storage to a memory limit as it grows, and returns the
// interrupt's error once that passes the limit.
func (sb *StringBuilder) checkLength(r *Realm) error {
	// The inline bytes are short and on the stack: only heap storage that
	// outgrew the last check's capacity needs the slow path.
	if len(sb.b)+len(sb.u) > int(sb.checked) {
		return r.checkBuilder(sb)
	}
	return nil
}

// capBytes is the size of the builder's heap storage.
func (sb *StringBuilder) capBytes() int { return cap(sb.b) + 2*cap(sb.u) }

// checkBuilder is checkLength's slow path, kept out of line so that
// checkLength inlines into the loops that call it: it runs at a builder's
// first check past its inline bytes and when the builder outgrew the
// capacity of the last check.
// Growing allocates the whole new capacity, which it charges.
//
//go:noinline
func (r *Realm) checkBuilder(sb *StringBuilder) error {
	if sb.Len() > maxStringLength {
		return r.invalidStringLength()
	}
	sb.checked = int32(min(sb.capUnits(), maxStringLength))
	if c := sb.capBytes(); c > 0 && r.mem() != nil {
		return r.reserve(c)
	}
	return nil
}

// capUnits is the capacity of the builder's storage in code units.
func (sb *StringBuilder) capUnits() int {
	if sb.b == nil && sb.u == nil {
		return len(sb.inline)
	}
	return cap(sb.b) + cap(sb.u)
}

// builtString is sb.String() for a producer's result, charging the
// result's header and the storage checkLength has not charged.
func (r *Realm) builtString(sb *StringBuilder) *String {
	if r.mem() != nil {
		n := int(sb.n)
		if sb.b != nil || sb.u != nil {
			n = 0
			if int(sb.checked) != sb.capUnits() {
				n = sb.capBytes()
			}
		}
		r.chargeString(n)
	}
	return sb.String()
}

// Len returns the number of code units written so far. Only one of the
// three stores holds them (spill and upgrade empty the one they leave), so
// the sum is the length of that one, without branches.
func (sb *StringBuilder) Len() int {
	return len(sb.u) + len(sb.b) + int(sb.n)
}

// ascii returns the ASCII bytes written so far (valid only while u == nil;
// the slice must not be retained across a write).
func (sb *StringBuilder) ascii() []byte {
	if sb.b != nil {
		return sb.b
	}
	return sb.inline[:sb.n]
}

func (sb *StringBuilder) upgrade() {
	if sb.u != nil {
		return
	}
	src := sb.ascii()
	u := make([]uint16, len(src), max(2*len(src), 16))
	for i, c := range src {
		u[i] = uint16(c)
	}
	sb.u = u
	sb.b = nil
	sb.n = 0
}

// WriteUnit appends one UTF-16 code unit.
func (sb *StringBuilder) WriteUnit(c uint16) {
	if sb.u == nil {
		if c < 0x80 {
			sb.WriteASCII(byte(c))
			return
		}
		sb.upgrade()
	}
	sb.u = append(sb.u, c)
}

// WriteASCII appends one byte that must be < 0x80.
func (sb *StringBuilder) WriteASCII(c byte) {
	if sb.u != nil {
		sb.u = append(sb.u, uint16(c))
		return
	}
	if sb.b == nil {
		if int(sb.n) < len(sb.inline) {
			sb.inline[sb.n] = c
			sb.n++
			return
		}
		sb.spill(2 * len(sb.inline))
	}
	sb.b = append(sb.b, c)
}

// writeASCIIBytes appends bytes that are all < 0x80. Within the heap
// slice's capacity it stores only the slice's length: storing it whole, as
// sb.b = append(sb.b, s...) does at every call, stores the data pointer,
// which costs a write barrier while the collector marks. (WriteASCII's
// append of one byte is compiled to store only the length already.)
func (sb *StringBuilder) writeASCIIBytes(s string) {
	if sb.u != nil {
		n := len(sb.u)
		if cap(sb.u)-n < len(s) {
			sb.u = slices.Grow(sb.u, len(s))
		}
		sb.u = sb.u[:n+len(s)]
		u := sb.u[n:][:len(s)]
		for i := range len(s) {
			u[i] = uint16(s[i])
		}
		return
	}
	if sb.b == nil {
		if int(sb.n)+len(s) <= len(sb.inline) {
			sb.n += int32(copy(sb.inline[sb.n:], s))
			return
		}
		sb.spill(max(2*len(sb.inline), int(sb.n)+len(s)))
	}
	// The length is stored before the copy, from the slice as loaded:
	// after the copy, a call, the slice would be loaded and checked again.
	if b, n := sb.b, len(sb.b); n+len(s) <= cap(b) {
		sb.b = sb.b[:n+len(s)]
		copy(b[n:n+len(s)], s)
		return
	}
	sb.b = append(sb.b, s...)
}

// WriteRune appends a code point, encoding surrogate pairs as needed.
func (sb *StringBuilder) WriteRune(r rune) {
	if r < 0x10000 {
		sb.WriteUnit(uint16(r))
		return
	}
	hi, lo := utf16.EncodeRune(r)
	sb.WriteUnit(uint16(hi))
	sb.WriteUnit(uint16(lo))
}

// WriteGoString appends a UTF-8 Go string.
func (sb *StringBuilder) WriteGoString(s string) {
	if sb.u == nil && isASCII(s) {
		sb.writeASCIIBytes(s)
		return
	}
	for _, r := range s {
		sb.WriteRune(r)
	}
}

// WriteString appends a JavaScript string.
func (sb *StringBuilder) WriteString(s *String) {
	if s.needsFlat() {
		sb.writeRope(s)
		return
	}
	if s.kind == strASCII {
		sb.writeASCIIBytes(s.s)
		return
	}
	sb.upgrade()
	sb.appendUnits(s.units())
}

// writeRope is WriteString of a rope or lazy string, out of line so that
// the view (flat) does not grow WriteString's frame.
//
//go:noinline
func (sb *StringBuilder) writeRope(s *String) {
	f := s.flat(new(String))
	if f.kind == strASCII {
		sb.writeASCIIBytes(f.s)
		return
	}
	sb.upgrade()
	sb.appendUnits(f.units())
}

// WriteUTF16 appends raw code units: one at a time until the first unit >=
// 0x80 has upgraded the builder, then the rest at once.
func (sb *StringBuilder) WriteUTF16(u []uint16) {
	for len(u) > 0 && sb.u == nil {
		sb.WriteUnit(u[0])
		u = u[1:]
	}
	sb.appendUnits(u)
}

// appendUnits appends u to the UTF-16 storage, storing only its length
// within its capacity (writeASCIIBytes).
func (sb *StringBuilder) appendUnits(u []uint16) {
	n := len(sb.u)
	if n+len(u) > cap(sb.u) {
		sb.u = slices.Grow(sb.u, len(u))
	}
	b := sb.u
	sb.u = sb.u[:n+len(u)]
	copy(b[n:n+len(u)], u)
}

// writeASCII2 appends the bytes a and b, both < 0x80: an escape.
func (sb *StringBuilder) writeASCII2(a, b byte) {
	if d, n := sb.b, len(sb.b); n+2 <= cap(d) {
		sb.b = sb.b[:n+2]
		d = d[:n+2]
		d[n], d[n+1] = a, b
		return
	}
	sb.WriteASCII(a)
	sb.WriteASCII(b)
}

// String finishes the builder and returns the result. The builder must not be
// reused without Reset.
func (sb *StringBuilder) String() *String {
	if sb.u != nil {
		u := sb.u
		sb.u = nil
		return FromUTF16(u)
	}
	if sb.b != nil {
		b := sb.b
		sb.b = nil
		return &String{s: bytesToString(b), n: int32(len(b)), kind: strASCII}
	}
	if sb.n == 0 {
		return emptyString
	}
	s := asciiStringCopy(sb.inline[:sb.n])
	sb.n = 0
	return s
}

// truncate drops the code units written after the first n.
func (sb *StringBuilder) truncate(n int) {
	switch {
	case sb.u != nil:
		sb.u = sb.u[:n]
	case sb.b != nil:
		sb.b = sb.b[:n]
	default:
		sb.n = int32(n)
	}
}

// Reset clears the builder for reuse, keeping no reference to the old buffer.
func (sb *StringBuilder) Reset() {
	sb.b = nil
	sb.u = nil
	sb.n = 0
	sb.checked = 0
}

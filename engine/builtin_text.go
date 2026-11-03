package engine

// TextEncoder and TextDecoder (WHATWG Encoding Standard §8), for UTF-8
// only: a TextDecoder label that names any other encoding is a RangeError.
// Their members follow Web IDL: the methods and getters are enumerable, the
// getters and methods check their receiver, and the dictionary options are
// read in order before the constructor or method runs.
//
// One deliberate departure: decode's input and encodeInto's destination may
// be, or view, a resizable ArrayBuffer or a growable SharedArrayBuffer.
// Their Web IDL types lack [AllowResizable], which makes that a TypeError
// today, but whatwg/encoding#362 adds it (the editor agreed in #344), and
// Node, Deno and Bun already accept such buffers. Each uses the length the
// buffer has when it gets to the bytes: decode after reading its options,
// encodeInto after converting the source.
//
// The two constructors are a late group of their own (lateGlobal); their
// intrinsics hang off the binary group's (binaryIntrinsics.text), which
// installText builds along for encode's Uint8Array.

import (
	"encoding/binary"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"
)

func init() {
	lateGlobal(StringKey(AtomTextEncoder), installText)
	lateGlobal(StringKey(AtomTextDecoder), installText)
}

var lateTextEncoder = lateIndex(StringKey(AtomTextEncoder))

// textIntrinsics are the intrinsics of TextEncoder and TextDecoder.
type textIntrinsics struct {
	TextEncoderPrototype, TextEncoderCtor *Object
	TextDecoderPrototype, TextDecoderCtor *Object
}

// textIntr returns the realm's text intrinsics, defining the TextEncoder
// group if the realm has yet to.
func (r *Realm) textIntr() *textIntrinsics {
	r.lateAt(lateTextEncoder)
	return r.binary.text
}

func (t *textIntrinsics) visit(visit func(*Object)) {
	visit(t.TextEncoderPrototype)
	visit(t.TextEncoderCtor)
	visit(t.TextDecoderPrototype)
	visit(t.TextDecoderCtor)
}

type textDefs struct {
	encoderGetters, decoderGetters []getterDef
	encoderMethods, decoderMethods []builtinDef
	encoderCall, decoderCall       NativeFunc
}

var textTables = sync.OnceValue(func() *textDefs {
	a := textNames()
	return &textDefs{
		encoderGetters: []getterDef{{name: a.encoding, get: textEncoderEncoding}},
		decoderGetters: []getterDef{
			{name: a.encoding, get: textDecoderEncoding},
			{name: a.fatal, get: textDecoderFatal},
			{name: a.ignoreBOM, get: textDecoderIgnoreBOM},
		},
		encoderMethods: []builtinDef{{a.encode, textEncoderEncode, 0}, {a.encodeInto, textEncoderEncodeInto, 2}},
		decoderMethods: []builtinDef{{a.decode, textDecoderDecode, 0}},
		encoderCall:    requireNew("TextEncoder"),
		decoderCall:    requireNew("TextDecoder"),
	}
})

// installText is the late installer of TextEncoder and TextDecoder.
func installText(r *Realm) {
	b := r.binaryIntr()
	if r.buildingShared {
		// The binary intrinsics may be published already: extend a copy.
		c := *b
		b = &c
		r.binary = b
	}
	t, d := &textIntrinsics{}, textTables()
	b.text = t
	const webIDL = attrDefault // Web IDL members are enumerable

	t.TextEncoderPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 5)
	t.TextEncoderCtor = r.newConstructor(AtomTextEncoder, 0, d.encoderCall, textEncoderConstruct, t.TextEncoderPrototype)
	r.installGettersAttrs(t.TextEncoderPrototype, d.encoderGetters, webIDL&^attrWritable)
	r.installBuiltinsAttrs(t.TextEncoderPrototype, d.encoderMethods, webIDL)
	r.installToStringTag(t.TextEncoderPrototype, AtomTextEncoder)

	t.TextDecoderPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 6)
	t.TextDecoderCtor = r.newConstructor(AtomTextDecoder, 0, d.decoderCall, textDecoderConstruct, t.TextDecoderPrototype)
	r.installGettersAttrs(t.TextDecoderPrototype, d.decoderGetters, webIDL&^attrWritable)
	r.installBuiltinsAttrs(t.TextDecoderPrototype, d.decoderMethods, webIDL)
	r.installToStringTag(t.TextDecoderPrototype, AtomTextDecoder)

	r.bindGlobal(AtomTextEncoder, ObjectValue(t.TextEncoderCtor))
	r.bindGlobal(AtomTextDecoder, ObjectValue(t.TextDecoderCtor))
}

// thisText implements the receiver check of the TextEncoder (class
// ClassTextEncoder) and TextDecoder members.
func thisText(r *Realm, this Value, class Class, method string) (*Object, error) {
	if this.IsObject() && this.AsObject().class == class {
		return this.AsObject(), nil
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// --- TextEncoder ------------------------------------------------------------------

func textEncoderConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	t := r.textIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, t.TextEncoderCtor, t.TextEncoderPrototype)
	if err != nil {
		return Undefined(), err
	}
	r.markPrototype(proto)
	return ObjectValue(r.newObject(ClassTextEncoder, r.rootShapeFor(proto))), nil
}

func textEncoderEncoding(r *Realm, this Value, args []Value) (Value, error) {
	if _, err := thisText(r, this, ClassTextEncoder, "TextEncoder.prototype.encoding"); err != nil {
		return Undefined(), err
	}
	return StringValue(textNames().utf8), nil
}

// textEncoderEncode implements TextEncoder.prototype.encode(input = ""): the
// UTF-8 bytes of the string, lone surrogates as U+FFFD, in a new Uint8Array.
func textEncoderEncode(r *Realm, this Value, args []Value) (Value, error) {
	if _, err := thisText(r, this, ClassTextEncoder, "TextEncoder.prototype.encode"); err != nil {
		return Undefined(), err
	}
	s := emptyString
	if v := Arg(args, 0); !v.IsUndefined() {
		var err error
		if s, err = r.ToString(v); err != nil {
			return Undefined(), err
		}
	}
	n, err := r.utf8Length(s)
	if err != nil {
		return Undefined(), err
	}
	o, err := r.allocTypedArray(r.binaryIntr().typedArrayPrototypes[elemUint8], elemUint8, int64(n))
	if err != nil {
		return Undefined(), err
	}
	if _, _, err := r.encodeUTF8(o.internal.(*typedArray).viewed(), s); err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// textEncoderEncodeInto implements TextEncoder.prototype.encodeInto(source,
// destination): the UTF-8 bytes of source written to the Uint8Array
// destination up to the first code point that does not fit, and the object
// {read, written} of the code units read and the bytes written.
func textEncoderEncodeInto(r *Realm, this Value, args []Value) (Value, error) {
	const method = "TextEncoder.prototype.encodeInto"
	if _, err := thisText(r, this, ClassTextEncoder, method); err != nil {
		return Undefined(), err
	}
	if len(args) < 2 {
		return Undefined(), r.TypeError("%s requires 2 arguments, but only %d present", method, len(args))
	}
	s, err := r.ToString(args[0])
	if err != nil {
		return Undefined(), err
	}
	var ta *typedArray
	if args[1].IsObject() {
		ta, _ = args[1].AsObject().internal.(*typedArray)
	}
	if ta == nil || ta.kind != elemUint8 {
		return Undefined(), r.TypeError("%s: the destination is not a Uint8Array", method)
	}
	read, written, err := r.encodeUTF8(ta.viewed(), s)
	if err != nil {
		return Undefined(), err
	}
	return r.readWritten(read, written), nil
}

// utf8Length returns the length of the UTF-8 encoding of s, lone surrogates
// counted as U+FFFD, honouring the interrupt flag on long strings.
func (r *Realm) utf8Length(s *String) (int, error) {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return f.Len(), nil
	}
	u, n := f.units(), 0
	for i := 0; i < len(u); {
		stop := min(len(u), i+copyBytesChunk)
		for ; i < stop; i++ {
			switch c := u[i]; {
			case c < 0x80:
				n++
			case c < 0x800:
				n += 2
			case c&0xFC00 == 0xD800 && i+1 < len(u) && u[i+1]&0xFC00 == 0xDC00:
				n += 4
				i++
			default:
				n += 3
			}
		}
		if i < len(u) {
			if err := r.CheckInterrupt(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// encodeUTF8 writes the UTF-8 encoding of s, lone surrogates as U+FFFD, to
// dst up to the first code point that does not fit, and returns the code
// units it read and the bytes it wrote. It honours the interrupt flag on
// long strings.
func (r *Realm) encodeUTF8(dst []byte, s *String) (read, written int, err error) {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		a := f.s
		n := min(len(a), len(dst))
		for read < n {
			end := min(n, read+copyBytesChunk)
			copy(dst[read:end], a[read:end])
			if read = end; read < n {
				if err := r.CheckInterrupt(); err != nil {
					return 0, 0, err
				}
			}
		}
		return n, n, nil
	}
	u := f.units()
	for read < len(u) {
		stop := min(len(u), read+copyBytesChunk)
		i, w := encodeUTF16Units(dst[written:], u, read, stop)
		read, written = i, written+w
		if read < stop {
			break // dst is full
		}
		if read < len(u) {
			if err := r.CheckInterrupt(); err != nil {
				return 0, 0, err
			}
		}
	}
	return read, written, nil
}

// encodeUTF16Units encodes the code points of u from index i up to one
// that starts at or after stop, or that does not fit dst, and returns the
// index after the last one it encoded and the bytes it wrote.
func encodeUTF16Units(dst []byte, u []uint16, i, stop int) (int, int) {
	w := 0
	for i < stop {
		c, units, size := rune(u[i]), 1, 3
		switch {
		case c < 0x80:
			if w == len(dst) {
				return i, w
			}
			dst[w] = byte(c)
			w++
			i++
			continue
		case c < 0x800:
			size = 2
		case c&0xFC00 == 0xD800 && i+1 < len(u) && u[i+1]&0xFC00 == 0xDC00:
			c = 0x10000 + (c-0xD800)<<10 + rune(u[i+1]) - 0xDC00
			units, size = 2, 4
		}
		if len(dst)-w < size {
			return i, w
		}
		w += utf8.EncodeRune(dst[w:], c) // a lone surrogate encodes as U+FFFD
		i += units
	}
	return i, w
}

// --- TextDecoder ------------------------------------------------------------------

// textDecoder is the payload of a TextDecoder: its options and, between the
// calls of a stream, the state of its UTF-8 decoder (do not flush and BOM
// seen, and the handler's bytes needed and seen, code point, and lower and
// upper boundary).
type textDecoder struct {
	fatal, ignoreBOM    bool
	doNotFlush, bomSeen bool
	needed, seen        uint8
	lower, upper        byte
	cp                  rune
}

// textDecoderObject co-allocates a TextDecoder with its payload.
type textDecoderObject struct {
	obj Object
	dec textDecoder
}

// resetSequence returns the UTF-8 handler to its state between sequences.
func (d *textDecoder) resetSequence() {
	d.needed, d.seen, d.cp = 0, 0, 0
	d.lower, d.upper = 0x80, 0xBF
}

// isUTF8Label reports whether label, stripped of ASCII whitespace and
// compared case-insensitively, is one of the labels of UTF-8.
func isUTF8Label(label *String) bool {
	a, ok := label.ASCII()
	if !ok {
		return false
	}
	switch strings.ToLower(strings.Trim(a, "\t\n\f\r ")) {
	case "unicode-1-1-utf-8", "unicode11utf8", "unicode20utf8", "utf-8", "utf8", "x-unicode20utf8":
		return true
	}
	return false
}

// dictionary checks the Web IDL dictionary argument v: nil for undefined or
// null (every member absent), a TypeError when v is no object.
func (r *Realm) dictionary(v Value, name string) (*Object, error) {
	if v.IsUndefined() || v.IsNull() {
		return nil, nil
	}
	if !v.IsObject() {
		return nil, r.TypeError("The provided value is not of type '%s'", name)
	}
	return v.AsObject(), nil
}

// boolMember reads the boolean member key of dictionary o, false when
// absent.
func (r *Realm) boolMember(o *Object, key *String) (bool, error) {
	if o == nil {
		return false, nil
	}
	v, err := o.GetProp(r, StringKey(key))
	return ToBoolean(v), err
}

// textDecoderConstruct implements new TextDecoder(label = "utf-8", options):
// the conversions of the label and the options, the prototype, then the
// label check.
func textDecoderConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	var label *String
	if v := Arg(args, 0); !v.IsUndefined() {
		var err error
		if label, err = r.ToString(v); err != nil {
			return Undefined(), err
		}
	}
	opts, err := r.dictionary(Arg(args, 1), "TextDecoderOptions")
	if err != nil {
		return Undefined(), err
	}
	a := textNames()
	fatal, err := r.boolMember(opts, a.fatal)
	if err != nil {
		return Undefined(), err
	}
	ignoreBOM, err := r.boolMember(opts, a.ignoreBOM)
	if err != nil {
		return Undefined(), err
	}
	t := r.textIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, t.TextDecoderCtor, t.TextDecoderPrototype)
	if err != nil {
		return Undefined(), err
	}
	if label != nil && !isUTF8Label(label) {
		return Undefined(), r.RangeError("The \"%s\" encoding is not supported", label.GoString())
	}
	r.markPrototype(proto)
	do := &textDecoderObject{dec: textDecoder{fatal: fatal, ignoreBOM: ignoreBOM}}
	r.chargeObject(unsafe.Sizeof(textDecoderObject{}))
	do.dec.resetSequence()
	o := &do.obj
	o.shape = r.rootShapeFor(proto)
	o.proto = proto
	o.class = ClassTextDecoder
	o.flags = flagExtensible
	o.internal = &do.dec
	return ObjectValue(o), nil
}

func thisTextDecoder(r *Realm, this Value, method string) (*textDecoder, error) {
	o, err := thisText(r, this, ClassTextDecoder, method)
	if err != nil {
		return nil, err
	}
	return o.internal.(*textDecoder), nil
}

func textDecoderEncoding(r *Realm, this Value, args []Value) (Value, error) {
	if _, err := thisTextDecoder(r, this, "TextDecoder.prototype.encoding"); err != nil {
		return Undefined(), err
	}
	return StringValue(textNames().utf8), nil
}

func textDecoderFatal(r *Realm, this Value, args []Value) (Value, error) {
	d, err := thisTextDecoder(r, this, "TextDecoder.prototype.fatal")
	if err != nil {
		return Undefined(), err
	}
	return Bool(d.fatal), nil
}

func textDecoderIgnoreBOM(r *Realm, this Value, args []Value) (Value, error) {
	d, err := thisTextDecoder(r, this, "TextDecoder.prototype.ignoreBOM")
	if err != nil {
		return Undefined(), err
	}
	return Bool(d.ignoreBOM), nil
}

// textDecoderDecode implements TextDecoder.prototype.decode(input, options):
// input is an ArrayBuffer, a SharedArrayBuffer or a view of one, whose
// bytes it reads after the options (a detached or out-of-bounds one has
// none); with {stream: true} an incomplete sequence at the end waits for
// the next call. With fatal, an invalid sequence throws a TypeError and
// drops the rest of the input; the stream goes on from the next call.
func textDecoderDecode(r *Realm, this Value, args []Value) (Value, error) {
	const method = "TextDecoder.prototype.decode"
	d, err := thisTextDecoder(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	var src *Object
	if v := Arg(args, 0); !v.IsUndefined() {
		if v.IsObject() {
			if _, ok := v.AsObject().BufferData(); ok {
				src = v.AsObject()
			}
		}
		if src == nil {
			return Undefined(), r.TypeError("%s: the input is not an ArrayBuffer or a view of one", method)
		}
	}
	opts, err := r.dictionary(Arg(args, 1), "TextDecodeOptions")
	if err != nil {
		return Undefined(), err
	}
	stream, err := r.boolMember(opts, textNames().stream)
	if err != nil {
		return Undefined(), err
	}
	var in []byte
	if src != nil {
		in, _ = src.BufferData()
	}
	s, err := r.decodeUTF8(d, in, stream)
	if err != nil {
		return Undefined(), err
	}
	return StringValue(s), nil
}

// asciiPrefix returns the length of the ASCII prefix of b.
func asciiPrefix(b []byte) int {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:])&0x8080808080808080 != 0 {
			break
		}
	}
	for i < len(b) && b[i] < 0x80 {
		i++
	}
	return i
}

// decodeUTF8 is one decode call of d over in: it starts a new stream unless
// the previous call streamed, runs the UTF-8 decoder over in, flushing it
// at the end unless this call streams, and serializes the code points
// (dropping a BOM that starts the stream unless d ignores BOMs). It honours
// the interrupt flag on large inputs.
func (r *Realm) decodeUTF8(d *textDecoder, in []byte, stream bool) (*String, error) {
	if !d.doNotFlush {
		d.bomSeen = false
		d.resetSequence()
	}
	d.doNotFlush = stream
	n := 0
	// A BOM that starts the stream is dropped first, so that ASCII after it
	// still takes the fast path. BOM seen is only set once the call
	// succeeds: the spec sets it when serializing, which a fatal error
	// never reaches.
	bom := false
	if d.needed == 0 {
		if !d.ignoreBOM && !d.bomSeen && len(in) >= 3 && in[0] == 0xEF && in[1] == 0xBB && in[2] == 0xBF {
			bom, in = true, in[3:]
		}
		for n < len(in) {
			end := min(len(in), n+copyBytesChunk)
			p := asciiPrefix(in[n:end])
			if n += p; n < end {
				break
			}
			if n < len(in) {
				if err := r.CheckInterrupt(); err != nil {
					return nil, err
				}
			}
		}
		if n > maxStringLength {
			return nil, r.invalidStringLength()
		}
		if n == len(in) {
			d.bomSeen = d.bomSeen || bom || n > 0
			if n == 0 {
				return emptyString, nil
			}
			s, dst := newASCIIBuf(n)
			r.chargeString(n)
			if err := r.copyBytes(dst, in); err != nil {
				return nil, err
			}
			return s, nil
		}
	}
	// No more code units than bytes, but for a sequence the previous call
	// began (one more); the limit is checked past a BOM the end may drop
	// (one that completes that sequence). A chunk of the loop stores at most
	// 2 units more than its bytes (the sequence it ends in may run 3 bytes
	// past it), so bounded by the room left, the check after it throws
	// before a store can run past u. k counts the units stored.
	u, k := make([]uint16, min(len(in), maxStringLength+1)+4), n
	r.chargeString(2 * len(u))
	for i := 0; i < n; {
		end := min(n, i+copyBytesChunk)
		for j, b := range in[i:end] {
			u[i+j] = uint16(b)
		}
		if i = end; i < n {
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
	}
	invalid := func() error {
		d.resetSequence()
		if d.fatal {
			return r.TypeError("The encoded data was not valid for encoding utf-8")
		}
		return nil
	}
	for i := n; i < len(in); {
		stop := min(len(in), i+copyBytesChunk, i+len(u)-k-3)
		for i < stop {
			b := in[i]
			if d.needed == 0 {
				if b < 0x80 {
					u[k] = uint16(b)
					k++
					i++
					continue
				}
				if c, size := utf8.DecodeRune(in[i:]); c != utf8.RuneError || size == 3 {
					k = putUTF16Rune(u, k, c)
					i += size
					continue
				}
				// An invalid or incomplete sequence: the Encoding
				// Standard's handler, a byte at a time.
				i++
				switch {
				case b >= 0xC2 && b <= 0xDF:
					d.needed, d.cp = 1, rune(b&0x1F)
				case b >= 0xE0 && b <= 0xEF:
					if b == 0xE0 {
						d.lower = 0xA0
					} else if b == 0xED {
						d.upper = 0x9F
					}
					d.needed, d.cp = 2, rune(b&0xF)
				case b >= 0xF0 && b <= 0xF4:
					if b == 0xF0 {
						d.lower = 0x90
					} else if b == 0xF4 {
						d.upper = 0x8F
					}
					d.needed, d.cp = 3, rune(b&0x7)
				default:
					if err := invalid(); err != nil {
						return nil, err
					}
					u[k] = 0xFFFD
					k++
				}
				continue
			}
			if b < d.lower || b > d.upper {
				// The byte starts over, after the error.
				if err := invalid(); err != nil {
					return nil, err
				}
				u[k] = 0xFFFD
				k++
				continue
			}
			i++
			d.lower, d.upper = 0x80, 0xBF
			d.cp = d.cp<<6 | rune(b&0x3F)
			if d.seen++; d.seen == d.needed {
				k = putUTF16Rune(u, k, d.cp)
				d.resetSequence()
			}
		}
		if k > maxStringLength+1 {
			return nil, r.invalidStringLength()
		}
		if i < len(in) {
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
	}
	if !stream && d.needed != 0 {
		if err := invalid(); err != nil {
			return nil, err
		}
		u[k] = 0xFFFD
		k++
	}
	u = u[:k]
	// The unit after the ASCII prefix is not ASCII, as the byte that ended
	// the prefix is not, so the result is ASCII when there is none (the
	// prefix before a sequence left for the next call, say), or when
	// nothing past a BOM the end drops is wider.
	wide := len(u) > n
	if (bom || len(u) > 0) && !d.ignoreBOM && !d.bomSeen {
		d.bomSeen = true
		if !bom && u[0] == 0xFEFF {
			u = u[1:]
			wide = false
			for i := 0; i < len(u) && !wide; {
				end := min(len(u), i+copyBytesChunk)
				wide = slices.ContainsFunc(u[i:end], func(c uint16) bool { return c >= 0x80 })
				if i = end; !wide && i < len(u) {
					if err := r.CheckInterrupt(); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if len(u) > maxStringLength {
		return nil, r.invalidStringLength()
	}
	if !wide {
		// Narrowed as FromUTF16 would, without its scan.
		s, dst := newASCIIBuf(len(u))
		for i := 0; i < len(u); {
			end := min(len(u), i+copyBytesChunk)
			for j, c := range u[i:end] {
				dst[i+j] = byte(c)
			}
			if i = end; i < len(u) {
				if err := r.CheckInterrupt(); err != nil {
					return nil, err
				}
			}
		}
		return s, nil
	}
	if cap(u)-len(u) > len(u)/4+64 {
		u = append([]uint16(nil), u...)
	}
	return utf16String(u), nil
}

// putUTF16Rune stores the UTF-16 encoding of the scalar value c at u[k:]
// and returns the index past it.
func putUTF16Rune(u []uint16, k int, c rune) int {
	if c < 0x10000 {
		u[k] = uint16(c)
		return k + 1
	}
	c -= 0x10000
	u[k], u[k+1] = uint16(0xD800+c>>10), uint16(0xDC00+c&0x3FF)
	return k + 2
}

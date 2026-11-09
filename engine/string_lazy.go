package engine

import (
	"sync"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"
)

// lazyStringMode fixes how retained bytes are decoded. Reads may populate
// caches, but never change the mode or the retained bytes. A lazy string's
// kind is strLazy plus its mode. The text modes come first
// (String.lazyTextMode).
type lazyStringMode uint8

const (
	// lazyText holds Go text: invalid UTF-8 bytes decode to U+FFFD, as
	// FromGoString replaces them.
	lazyText lazyStringMode = iota

	// lazyWTF8 holds well-formed WTF-8 text that may encode lone UTF-16
	// surrogates, which decoding preserves. Only jsonParser constructs it,
	// after JSONParseGoString checked utf8.ValidString (falling back to
	// FromGoString on invalid bytes) or String.wtf8 encoded a JS string's
	// UTF-16 units; arbitrary Go strings may contain ill-formed bytes and
	// must use lazyText so decoding does not size them with wtf8Units.
	lazyWTF8

	// lazyJSONContents holds JSON contents without quotes; strSlow checked
	// their JSON syntax. Byte runs between escapes come from the same
	// well-formed jsonParser source as lazyWTF8 and are sized with wtf8Units.
	lazyJSONContents

	// lazyJSONVerbatim holds valid UTF-8 JSON contents in stringify's exact
	// escape spelling. AppendJSON can copy them; content reads still unescape.
	lazyJSONVerbatim
)

// Facts about the retained bytes of a text mode string, cached in
// lazyStringData.facts. A fact pair with neither bit set is unchecked.
const (
	factUTF8Valid uint32 = 1 << iota
	factUTF8Invalid
	factJSONPlain   // ASCII with nothing JSON quoting escapes
	factJSONEscapes // not factJSONPlain
)

// lazyStringData makes concurrent readers of a lazy string decode it once
// (resolved), which publishes the decoded String in the String's right
// field, and facts holds what is known of its source; the String's hash
// field caches its hash atomically. The String's n, kind and flags, set
// when it is made, hold its length, mode and whether it decodes to UTF-16
// (strLazyWide). Without a share root (strLazyRooted), the String and its
// data take one 64-byte allocation.
type lazyStringData struct {
	once  sync.Once
	facts atomic.Uint32
}

func (s *String) lazyMode() lazyStringMode { return lazyStringMode(s.kind - strLazy) }
func (s *String) lazyTextMode() bool       { return s.lazyMode() <= lazyWTF8 }

// validLazyUTF8 reports whether text, the retained bytes, is the UTF-8 of
// the string's content: text mode bytes that are valid UTF-8 (which encodes
// no surrogates). The check runs once.
func (s *String) validLazyUTF8() bool {
	if !s.lazyTextMode() {
		return false
	}
	d := s.lazyData()
	f := d.facts.Load()
	if f&(factUTF8Valid|factUTF8Invalid) == 0 {
		f = factUTF8Invalid
		if utf8.ValidString(s.s) {
			f = factUTF8Valid
		}
		d.facts.Or(f)
	}
	return f&factUTF8Valid != 0
}

// plainLazyJSON reports whether text can be copied between JSON quotes as
// ASCII without escaping. Only text mode strings of at least jsonPlainMin
// bytes are scanned; false can also mean the mode or length is ineligible.
func (s *String) plainLazyJSON() bool {
	if len(s.s) < jsonPlainMin || !s.lazyTextMode() {
		return false
	}
	d := s.lazyData()
	f := d.facts.Load()
	if f&(factJSONPlain|factJSONEscapes) == 0 {
		ascii, plain := jsonASCII(s.s)
		f = factJSONEscapes
		if ascii {
			f = factUTF8Valid | factJSONEscapes
			if plain {
				f = factUTF8Valid | factJSONPlain
			}
		}
		d.facts.Or(f)
	}
	return f&factJSONPlain != 0
}

// resolved decodes text once; every caller waits for the same result, and
// the original String keeps its source. Decoding, on any goroutine, charges
// no account: a memory limit charged the decoded storage, if it charges
// it, when the string was made (accountLazy).
func (s *String) resolved() *String {
	s.lazyData().once.Do(func() {
		// Go text with invalid bytes is the one source that is not
		// well-formed: the count when it was made checked it (facts).
		invalid := s.lazyMode() == lazyText && s.lazyData().facts.Load()&factUTF8Valid == 0
		decoded := decodeLazyStringSized(s.s, s.lazyMode(), int(s.n), s.lazyWide(), invalid)
		atomic.StorePointer(s.rightWord(), unsafe.Pointer(decoded))
	})
	return (*String)(atomic.LoadPointer(s.rightWord()))
}

// lazyPublished returns the decoded String once resolved has published it,
// nil before, without waiting for a decoding in progress (the live
// measurement).
func (s *String) lazyPublished() *String {
	return (*String)(atomic.LoadPointer(s.rightWord()))
}

func (s *String) lazyWide() bool { return s.flags&strLazyWide != 0 }

// lazyDecodedSize is the heap storage, header included, that decoding s
// retains: what converting its source eagerly (FromGoString, or the JSON
// parser's own decoding) allocates.
func (s *String) lazyDecodedSize() int {
	switch {
	case s.lazyWide():
		return stringHeaderSize + 2*int(s.n)
	case s.lazyTextMode():
		return stringHeaderSize // ASCII text: the units are the source's bytes
	}
	return stringHeaderSize + int(s.n)
}

// hostUnits counts the code units the Go text decodes to and reports
// whether one is not ASCII, and the UTF-8 fact the count learned. Valid
// UTF-8 is checked and counted in one pass (utf8Units); text with invalid
// bytes counts as range does, one unit (U+FFFD) per invalid byte.
func hostUnits(text string) (n int, wide bool, facts uint32) {
	if isASCIILong(text) { // 32 bytes a step, before the per-sequence pass
		return len(text), false, factUTF8Valid
	}
	if n, ok := utf8Units(text); ok {
		return n, true, factUTF8Valid
	}
	n = 0
	for _, r := range text {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n, true, factUTF8Invalid
}

// decodeLazyStringSized decodes text to the n units counted when the string
// was made, UTF-16 if wide; invalid is Go text with invalid bytes. n must
// equal the units written: newUTF16Buf(n) binds the String to u of length n
// before append fills u[:0] in place, so an undercount reallocates inside
// append and truncates the tail, and an overcount leaves trailing zero
// units. The counts agree with what is written: appendWellFormed writes
// what wtf8Units and utf8Units count, appendUTF16 what range does.
func decodeLazyStringSized(text string, mode lazyStringMode, n int, wide, invalid bool) *String {
	if mode == lazyJSONContents || mode == lazyJSONVerbatim {
		return decodeJSONContents(text, n, wide)
	}
	if !wide {
		return asciiString(text)
	}
	s, u := newUTF16Buf(n)
	if invalid {
		appendUTF16(u[:0], text)
	} else {
		appendWellFormed(u[:0], text)
	}
	return s
}

// lazyStringBlock allocates a lazy String and its data together.
type lazyStringBlock struct {
	value String
	data  lazyStringData
}

// rootedLazyData is the data of a lazy string a parse under a memory limit
// made (strLazyRooted): its share root, the parse's text, immutable after
// publication and reached through p, so it does not depend on where the
// String header lives.
type rootedLazyData struct {
	lazyStringData
	root *String
}

type rootedLazyStringBlock struct {
	value String
	data  rootedLazyData
}

// newLazyStringSized makes a lazy string of text that decodes to n units,
// UTF-16 if wide; with rooted, its data has room for a share root.
func newLazyStringSized(text string, mode lazyStringMode, rooted bool, n int, wide bool) *String {
	if len(text) == 0 {
		return emptyString
	}
	kind := strLazy + uint8(mode)
	var flags uint8
	if wide {
		flags = strLazyWide
	}
	if rooted {
		b := &rootedLazyStringBlock{}
		b.value = String{s: text, p: unsafe.Pointer(&b.data), n: int32(n), kind: kind, flags: flags | strLazyRooted}
		return &b.value
	}
	b := &lazyStringBlock{}
	b.value = String{s: text, p: unsafe.Pointer(&b.data), n: int32(n), kind: kind, flags: flags}
	return &b.value
}

// newHostLazyString makes a lazy string of the Go text, counting its units
// and keeping the UTF-8 fact the count learned.
func newHostLazyString(text string) *String {
	n, wide, facts := hostUnits(text)
	s := newLazyStringSized(text, lazyText, false, n, wide)
	if s.isLazy() {
		s.lazyData().facts.Store(facts)
	}
	return s
}

func (s *String) isLazy() bool { return s.kind >= strLazy }

func (s *String) lazyData() *lazyStringData { return (*lazyStringData)(s.p) }

// rootedData is the rootedLazyData of a lazy string a parse under a
// memory limit made, nil for any other.
func (s *String) rootedData() *rootedLazyData {
	if s.flags&strLazyRooted != 0 {
		return (*rootedLazyData)(s.p)
	}
	return nil
}

func (s *String) lazyShareRoot() *String {
	if d := s.rootedData(); d != nil {
		return d.root
	}
	return nil
}

func (s *String) lazyBlockSize() int {
	if s.flags&strLazyRooted != 0 {
		return int(unsafe.Sizeof(rootedLazyStringBlock{}))
	}
	return int(unsafe.Sizeof(lazyStringBlock{}))
}

// plainView returns a flat ASCII String of the source of the lazy s, known
// plain (plainLazyJSON), without decoding s: its decoding if one was
// published, else the String decoding would publish, which s does not keep.
func (s *String) plainView() *String {
	if f := s.lazyPublished(); f != nil {
		return f
	}
	return &String{s: s.s, n: s.n, kind: strASCII, json: jsonPlain}
}

// sourceUTF8 returns the retained bytes of a lazy string when they are the
// UTF-8 of its content, which exports then share without decoding.
func (s *String) sourceUTF8() (string, bool) {
	if s.isLazy() && s.validLazyUTF8() {
		return s.s, true
	}
	return "", false
}

// sameLazyContent reports that two lazy strings retaining the same bytes
// text decode to the same content: one mode, both JSON modes (one decoder),
// or text modes and valid UTF-8, which the text modes decode alike.
func sameLazyContent(a, b *String) bool {
	if a.lazyMode() == b.lazyMode() {
		return true
	}
	if a.lazyTextMode() != b.lazyTextMode() {
		return false
	}
	return !a.lazyTextMode() || a.validLazyUTF8()
}

// lazyHash caches Hash atomically in the hash field.
func (s *String) lazyHash() uint32 {
	if h := atomic.LoadUint32(&s.hash); h != 0 {
		return h
	}
	h := s.resolved().contentHash()
	atomic.StoreUint32(&s.hash, h)
	return h
}

// FromHostString is FromGoString with the decoding deferred: a string
// shorter than jsonPlainMin that is ASCII is converted as FromGoString
// converts it, and any other keeps its bytes until its content is first
// read; its length is counted now. GoString of it shares the bytes when they
// are valid UTF-8.
func FromHostString(text string) *String {
	if len(text) < jsonPlainMin && isASCII(text) {
		return asciiString(text)
	}
	return newHostLazyString(text)
}

// lazyStringSized makes the lazy string of a parse's text that decodes to n
// units, UTF-16 if wide, charged as accountLazy says. Under a memory limit
// the parse's text is its share root (shareText), as for plain's strings,
// so the live measurement counts the text once.
func (r *Realm) lazyStringSized(text string, mode lazyStringMode, n int, wide bool) *String {
	m := r.mem()
	s := newLazyStringSized(text, mode, m != nil, n, wide)
	if m != nil && s.isLazy() {
		s.rootedData().root = m.jsonText
	}
	r.accountLazy(s)
	return s
}

// accountLazy charges a lazy s a parse or a host container made to the
// realm's memory limit: its block, and the storage its decoding retains,
// which converting it eagerly charges when it is made. Whether the string
// is ever read then changes nothing the limit sees. Other strings and
// realms without a limit are unchanged.
func (r *Realm) accountLazy(s *String) {
	if r.mem() == nil || !s.isLazy() {
		return
	}
	r.chargeString(s.lazyBlockSize() - stringHeaderSize + s.lazyDecodedSize())
}

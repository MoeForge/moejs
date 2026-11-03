package engine

// Intl.Segmenter (ECMA-402 §19): grapheme, word and sentence segmentation
// of UAX #29 (segment.go) with no locale tailoring, so every CLDR locale is
// available and segments alike. Without dictionaries, words of scripts
// written without spaces (Chinese, Japanese, Thai, ...) are the UAX #29
// default: one per ideograph, unlike ICU's dictionary words.
//
// A Segments object keeps the flat code units of its string (segText); its
// iterators and containing step the break algorithms from an index, so a
// segment costs its data object and, unless it is one code point
// (segmentString), its substring.

import (
	"sync/atomic"
	"unsafe"
)

// intlSegmenter is the internal of an Intl.Segmenter.
type intlSegmenter struct {
	locale *String
	gran   segGranularity
}

// intlSegments is the internal of a Segments object and of a segment
// iterator, whose next segment starts at pos. shape is the shape of its
// segment data objects in the realm. A Segments object remembers the
// segment containing found last, [lastStart, lastEnd) (lastEnd 0 for none),
// so that a caller walking the text costs a step per segment, as ICU keeps
// the boundaries around its last position. data and wordData are the data
// objects allocated together and not handed out yet (segmentData).
type intlSegments struct {
	str          *String
	text         segText
	pos          int
	shape        *Shape
	data         []segmentDataObject
	wordData     []wordSegmentDataObject
	lastStart    int32
	lastEnd      int32
	gran         segGranularity
	lastWordLike bool
}

func installIntlSegmenter(r *Realm, x *intlIntrinsics, a *intlAtoms) {
	x.SegmenterPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 4)
	x.SegmenterCtor = r.newConstructor(a.segmenterName, 0, requireNew("Intl.Segmenter"), intlSegmenterConstruct, x.SegmenterPrototype)
	r.installBuiltins(x.SegmenterCtor, []builtinDef{{a.supportedLocalesOf, intlSupportedLocalesOf, 1}})
	r.installBuiltins(x.SegmenterPrototype, []builtinDef{
		{a.resolvedOptions, intlSegmenterResolvedOptions, 0},
		{a.segment, intlSegmenterSegment, 1},
	})
	r.installToStringTag(x.SegmenterPrototype, a.segmenterTag)

	x.SegmentsPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 2)
	r.installBuiltins(x.SegmentsPrototype, []builtinDef{{a.containing, intlSegmentsContaining, 1}})
	r.installSymbolMethod(x.SegmentsPrototype, SymIterator, atomIteratorFn, intlSegmentsIterator, 0, attrHidden)

	x.SegmentIteratorPrototype = r.newIntrinsic(ClassObject, r.IteratorPrototype, 2)
	x.segmentIterNextFn = r.NewNativeFunction(AtomNext, 0, intlSegmentIteratorNext)
	r.installOrReplace(x.SegmentIteratorPrototype, StringKey(AtomNext), propCell{value: ObjectValue(x.segmentIterNextFn), attrs: attrHidden})
	r.installToStringTag(x.SegmentIteratorPrototype, a.segmentIteratorTag)
}

// intlSegmenterConstruct implements Intl.Segmenter(locales, options).
func intlSegmenterConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	const ctor = "Intl.Segmenter"
	x := r.intlIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, x.SegmenterCtor, x.SegmenterPrototype)
	if err != nil {
		return Undefined(), err
	}
	requested, err := r.canonicalizeLocaleList(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	options, err := r.getOptionsObject(intlArg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if _, err := r.intlLocaleMatcher(options, ctor); err != nil {
		return Undefined(), err
	}
	locale := r.intlResolveLocale(requested)
	a := intlNames()
	g, err := r.intlStringOption(options, ctor, a.granularity, a.grapheme, a.word, a.sentence)
	if err != nil {
		return Undefined(), err
	}
	s := &intlSegmenter{locale: r.intlString(locale)}
	switch g {
	case a.word:
		s.gran = segWord
	case a.sentence:
		s.gran = segSentence
	}
	r.markPrototype(proto)
	o := r.newObject(ClassIntlSegmenter, r.rootShapeFor(proto))
	o.internal = s
	return ObjectValue(o), nil
}

// intlSegmenterResolvedOptions implements
// Intl.Segmenter.prototype.resolvedOptions().
func intlSegmenterResolvedOptions(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisIntl[*intlSegmenter](r, this, ClassIntlSegmenter, "Intl.Segmenter.prototype.resolvedOptions")
	if err != nil {
		return Undefined(), err
	}
	a := intlNames()
	g := a.grapheme
	switch s.gran {
	case segWord:
		g = a.word
	case segSentence:
		g = a.sentence
	}
	o := r.NewObjectCap(2)
	o.addNamed(r, StringKey(a.locale), propCell{value: StringValue(s.locale), attrs: attrDefault})
	o.addNamed(r, StringKey(a.granularity), propCell{value: StringValue(g), attrs: attrDefault})
	return ObjectValue(o), nil
}

// intlSegmenterSegment implements Intl.Segmenter.prototype.segment(string)
// (CreateSegmentsObject).
func intlSegmenterSegment(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisIntl[*intlSegmenter](r, this, ClassIntlSegmenter, "Intl.Segmenter.prototype.segment")
	if err != nil {
		return Undefined(), err
	}
	str, err := r.ToString(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	sg := &intlSegments{str: str, gran: s.gran, shape: r.segmentDataShape(s.gran)}
	if a, ok := str.ASCII(); ok {
		sg.text.ascii = a
	} else {
		sg.text.u = str.UTF16()
	}
	o := r.newObject(ClassIntlSegments, r.rootShapeFor(r.intlIntr().SegmentsPrototype))
	o.internal = sg
	return ObjectValue(o), nil
}

// intlSegmentsContaining implements %IntlSegmentsPrototype%.containing(index).
func intlSegmentsContaining(r *Realm, this Value, args []Value) (Value, error) {
	sg, err := thisIntl[*intlSegments](r, this, ClassIntlSegments, "%Segments.prototype%.containing")
	if err != nil {
		return Undefined(), err
	}
	n, err := r.ToIntegerOrInfinity(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if n < 0 || n >= float64(sg.text.len()) {
		return Undefined(), nil
	}
	i := int(n)
	start, end, wordLike := int(sg.lastStart), int(sg.lastEnd), sg.lastWordLike
	if i >= end && end != 0 {
		// The segment after the last one, which a walk reaches next.
		start = end
		end, wordLike = segNext(sg.gran, &sg.text, start)
	}
	if i < start || i >= end {
		start = segStart(sg.gran, &sg.text, i)
		end, wordLike = segNext(sg.gran, &sg.text, start)
	}
	sg.lastStart, sg.lastEnd, sg.lastWordLike = int32(start), int32(end), wordLike
	return r.segmentData(sg, start, end, wordLike), nil
}

// intlSegmentsIterator implements %IntlSegmentsPrototype%[@@iterator]()
// (CreateSegmentIterator).
func intlSegmentsIterator(r *Realm, this Value, args []Value) (Value, error) {
	sg, err := thisIntl[*intlSegments](r, this, ClassIntlSegments, "%Segments.prototype%[@@iterator]")
	if err != nil {
		return Undefined(), err
	}
	it := *sg
	it.pos = 0
	it.data, it.wordData = nil, nil
	o := r.newObject(ClassIntlSegmentIterator, r.rootShapeFor(r.intlIntr().SegmentIteratorPrototype))
	o.internal = &it
	return ObjectValue(o), nil
}

// intlSegmentIteratorNext implements %IntlSegmentIteratorPrototype%.next().
func intlSegmentIteratorNext(r *Realm, this Value, args []Value) (Value, error) {
	it, err := thisIntl[*intlSegments](r, this, ClassIntlSegmentIterator, "%SegmentIterator.prototype%.next")
	if err != nil {
		return Undefined(), err
	}
	v, done := it.step(r)
	return r.createIterResult(v, done), nil
}

// step advances a segment iterator: the data object of its next segment,
// or done.
func (it *intlSegments) step(r *Realm) (Value, bool) {
	start := it.pos
	if start >= it.text.len() {
		return Undefined(), true
	}
	// segNext, inline: a call less per segment.
	var end int
	var wordLike bool
	switch it.gran {
	case segGrapheme:
		end = segGraphemeNext(&it.text, start)
	case segWord:
		end, wordLike = segWordNext(&it.text, start)
	default:
		end = segSentenceNext(&it.text, start)
	}
	it.pos = end
	return r.segmentData(it, start, end, wordLike), false
}

// segmentIterator returns the payload of it, a segment iterator whose next
// is next, when next is the original: stepping it directly is
// unobservable (iterStep).
func (r *Realm) segmentIterator(it, next Value) *intlSegments {
	if it.IsObject() && next.IsObject() {
		if o := it.AsObject(); o.class == ClassIntlSegmentIterator {
			if x := r.intl; x != nil && next.AsObject() == x.segmentIterNextFn {
				return o.internal.(*intlSegments)
			}
		}
	}
	return nil
}

// segmentDataObject co-allocates a segment data object with its slots:
// segment, index and input, and isWordLike for word granularity.
type segmentDataObject struct {
	obj   Object
	slots [3]Value
}

type wordSegmentDataObject struct {
	obj   Object
	slots [4]Value
}

// segmentDataBatch is how many data objects a Segments object or an
// iterator allocates together at most: one allocation of 1,008 or 960
// bytes (with the allocator's header, the 1,024-byte class) in place of
// 7 or 6, whose per-object work in the allocator was 40 % of a grapheme's
// instructions besides the loop running it.
const (
	segmentDataBatch     = 7
	wordSegmentDataBatch = 6
)

// segmentDataShape returns the shape of the segment data objects of a
// granularity, which Segments objects keep.
func (r *Realm) segmentDataShape(g segGranularity) *Shape {
	a := intlNames()
	s := r.plainRoot.addProperty(r, StringKey(a.segment), attrDefault).
		addProperty(r, StringKey(AtomIndex), attrDefault).
		addProperty(r, StringKey(AtomInput), attrDefault)
	if g == segWord {
		s = s.addProperty(r, StringKey(a.isWordLike), attrDefault)
	}
	return s
}

// segmentData implements CreateSegmentDataObject for the segment
// [start, end). Its object comes with its slots from sg's batch, and its
// string from segmentString.
//
// A batch holds at most as many objects as segments may follow, and stays
// alive while any of them does: a script that keeps one data object keeps
// up to its batch's 1 KiB. A realm with a memory limit allocates each object
// on its own instead, so that the live measurement, which sees objects and
// not batches, counts what they retain.
func (r *Realm) segmentData(sg *intlSegments, start, end int, wordLike bool) Value {
	var o *Object
	var slots *[3]Value
	lz := r.lazy
	limited := lz != nil && lz.mem != nil
	if sg.gran == segWord {
		var d *wordSegmentDataObject
		switch {
		case limited:
			d = &wordSegmentDataObject{}
			r.chargeObject(unsafe.Sizeof(wordSegmentDataObject{}))
		case len(sg.wordData) != 0:
			d = &sg.wordData[0]
			sg.wordData = sg.wordData[1:]
		default:
			b := make([]wordSegmentDataObject, min(wordSegmentDataBatch, sg.text.len()-end+1))
			d, sg.wordData = &b[0], b[1:]
		}
		d.slots[3] = Bool(wordLike)
		o, slots = &d.obj, (*[3]Value)(d.slots[:3])
		o.slots = d.slots[:]
	} else {
		var d *segmentDataObject
		switch {
		case limited:
			d = &segmentDataObject{}
			r.chargeObject(unsafe.Sizeof(segmentDataObject{}))
		case len(sg.data) != 0:
			d = &sg.data[0]
			sg.data = sg.data[1:]
		default:
			b := make([]segmentDataObject, min(segmentDataBatch, sg.text.len()-end+1))
			d, sg.data = &b[0], b[1:]
		}
		o, slots = &d.obj, &d.slots
		o.slots = d.slots[:]
	}
	o.shape = sg.shape
	o.proto = r.ObjectPrototype
	o.class = ClassObject
	o.flags = flagExtensible
	var seg *String
	if c := sg.text.at(start); end-start == 1 && c < 0x80 {
		seg = singleCharStrings[c]
	} else {
		seg = r.segmentString(sg, start, end)
	}
	slots[0] = StringValue(seg)
	slots[1] = IntValue(start)
	slots[2] = StringValue(sg.str)
	return ObjectValue(o)
}

// segmentString returns the string of the segment [start, end): a shared
// one when the segment is one code point (codePointString), a substring
// otherwise.
func (r *Realm) segmentString(sg *intlSegments, start, end int) *String {
	switch end - start {
	case 1:
		if s := codePointString(rune(sg.text.at(start))); s != nil {
			return s
		}
	case 2:
		if u := sg.text.u; u != nil && u[start]&0xFC00 == 0xD800 && u[start+1]&0xFC00 == 0xDC00 {
			c := rune(u[start])<<10 + rune(u[start+1]) - (0xD800<<10 + 0xDC00 - 0x10000)
			if s := codePointString(c); s != nil {
				return s
			}
		}
	}
	return r.substring(sg.str, start, end)
}

// The strings of one code point, shared by every realm and goroutine:
// ASCII's are singleCharStrings, the others are made on first use, in pages
// of 256 code points, and never written again. Each is made with its hash
// (Hash would otherwise store it from every goroutine that hashes one) and
// marked strConst, so the live measurement does not count them, as it skips
// the constants of shared code. The cache is bounded: past
// codePointStringsMax strings or codePointPagesMax pages (1.5 MiB in all),
// codePointString returns nil and the caller allocates its string.
type (
	codePointPage  [256]atomic.Pointer[codePointStr]
	codePointPlane [256]atomic.Pointer[codePointPage]
)

// codePointStr is a string of one code point allocated with its units.
type codePointStr struct {
	String
	u [2]uint16
}

const (
	codePointStringsMax = 1 << 14 // 64 bytes each
	codePointPagesMax   = 256     // 2 KiB each
)

var (
	codePointPlanes                  [17]atomic.Pointer[codePointPlane]
	codePointStrings, codePointPages atomic.Int32
)

// codePointString returns the shared string of the code point c, a lone
// surrogate included, or nil when the cache is full.
func codePointString(c rune) *String {
	if c < 0x80 {
		return singleCharStrings[c]
	}
	if pl := codePointPlanes[c>>16].Load(); pl != nil {
		if pg := pl[c>>8&255].Load(); pg != nil {
			if s := pg[c&255].Load(); s != nil {
				return &s.String
			}
		}
	}
	return newCodePointString(c)
}

// newCodePointString makes the string of c for codePointString, the plane
// and the page that hold it on the way, unless the cache is full. Two
// goroutines may make one at once: the first to publish it wins.
func newCodePointString(c rune) *String {
	pl := codePointPlanes[c>>16].Load()
	if pl == nil {
		codePointPlanes[c>>16].CompareAndSwap(nil, new(codePointPlane))
		pl = codePointPlanes[c>>16].Load()
	}
	page := &pl[c>>8&255]
	pg := page.Load()
	if pg == nil {
		if codePointPages.Load() >= codePointPagesMax {
			return nil
		}
		if page.CompareAndSwap(nil, new(codePointPage)) {
			codePointPages.Add(1)
		}
		pg = page.Load()
	}
	if codePointStrings.Load() >= codePointStringsMax {
		return nil
	}
	x := &codePointStr{}
	n := 1
	if c < 0x10000 {
		x.u[0] = uint16(c)
	} else {
		x.u = [2]uint16{uint16(0xD800 + (c-0x10000)>>10), uint16(0xDC00 + (c-0x10000)&0x3FF)}
		n = 2
	}
	x.String = String{p: unitsPtr(x.u[:n]), n: int32(n), kind: strUTF16, flags: strConst}
	x.hash = x.contentHash()
	if pg[c&255].CompareAndSwap(nil, x) {
		codePointStrings.Add(1)
	}
	return &pg[c&255].Load().String
}

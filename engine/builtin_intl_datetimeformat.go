package engine

// The skeleton and hour cycle helpers are ported from V8's
// src/objects/js-date-time-format.cc (BSD-3-Clause), the style patterns
// from ICU's SimpleDateFormat (Unicode License v3); see THIRD_PARTY_NOTICES.

// Intl.DateTimeFormat (ECMA-402 §11) with the Gregorian calendar, whose
// output equals V8's with ICU 78: the constructor reads its options in the
// specification's order and resolves the locale with the "ca", "hc" and "nu"
// keys; the pattern of the options comes from the port of ICU's
// DateTimePatternGenerator (intl_date_pattern.go) as V8 asks for it (a
// skeleton of the options, the hour cycle replaced afterwards), the styles'
// patterns as ICU's SimpleDateFormat builds them; formatting is
// intl_date_format.go's, ranges intl_date_range.go's, time zones
// intl_tz.go's.
//
// Only the Gregorian calendar is available: "ca" resolves to "gregory"
// whatever the locale or the calendar option asks for.

import (
	"math"
	"strings"
	"sync"
	"unsafe"
)

// intlDateRequired and intlDateDefaults are the required and defaults
// arguments of CreateDateTimeFormat.
type (
	intlDateRequired uint8
	intlDateDefaults uint8
)

const (
	intlRequireAny intlDateRequired = iota
	intlRequireDate
	intlRequireTime
)

const (
	intlDefaultsAll intlDateDefaults = iota
	intlDefaultsDate
	intlDefaultsTime
)

// intlDateTimeFormat is the internal of an Intl.DateTimeFormat.
type intlDateTimeFormat struct {
	locale    *String // [[Locale]]
	numbering *String // [[NumberingSystem]]
	zoneID    *String // [[TimeZone]]
	dataIndex int
	data      *intlDateData
	digits    *[10]rune // nil for latn
	zone      *intlZone
	// hourCycle is [[HourCycle]] as V8 keeps it: 0 when the pattern has no
	// hour, else K (h11), h (h12), H (h23) or k (h24).
	hourCycle byte
	// hourChar is the pattern generator's default hour letter and decimal
	// its decimal separator.
	hourChar byte
	// region is the locale's region, whose reference zones the generic zone
	// names take, NUL-padded (none: the data locale's likely region); it
	// fits the padding before decimal.
	region  [3]byte
	decimal string
	// dateStyle and timeStyle are 0 (undefined) or 1 + full to short.
	dateStyle, timeStyle int8
	pattern              string
	items                []intlDateItem
	hasMinute, hasSecond bool
	boundFormat          *Object
	rng                  *intlDateRange // formatRange's, found or built on first use
}

// regionName returns f.region as a string, without allocating: the bytes
// do not change once the formatter is made.
func (f *intlDateTimeFormat) regionName() string {
	n := 0
	for n < len(f.region) && f.region[n] != 0 {
		n++
	}
	if n == 0 {
		return ""
	}
	return unsafe.String(&f.region[0], n)
}

// intlHourCycles maps the hour letters to the hourCycle values and back.
func intlHourCycleName(c byte) *String {
	a := intlNames()
	switch c {
	case 'K':
		return a.h11
	case 'h':
		return a.h12
	case 'H':
		return a.h23
	case 'k':
		return a.h24
	}
	return nil
}

func intlHourCycleChar(name string) byte {
	switch name {
	case "h11":
		return 'K'
	case "h12":
		return 'h'
	case "h23":
		return 'H'
	case "h24":
		return 'k'
	}
	return 0
}

func installIntlDateTimeFormat(r *Realm, x *intlIntrinsics) {
	a := intlNames()
	d := intlDateNames()
	x.DateTimeFormatPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 8)
	x.DateTimeFormatCtor = r.newConstructor(d.dateTimeFormatName, 0, intlDateTimeFormatCall, intlDateTimeFormatConstruct, x.DateTimeFormatPrototype)
	r.installBuiltins(x.DateTimeFormatCtor, []builtinDef{{a.supportedLocalesOf, intlSupportedLocalesOfData, 1}})
	r.installGetters(x.DateTimeFormatPrototype, []getterDef{{d.format, d.getFormat, intlDateTimeFormatFormatGetter}})
	r.installBuiltins(x.DateTimeFormatPrototype, []builtinDef{
		{d.formatToParts, intlDateTimeFormatFormatToParts, 1},
		{a.resolvedOptions, intlDateTimeFormatResolvedOptions, 0},
		{d.formatRange, intlDateTimeFormatFormatRange, 2},
		{d.formatRangeToParts, intlDateTimeFormatFormatRangeToParts, 2},
	})
	r.installToStringTag(x.DateTimeFormatPrototype, d.dateTimeFormatTag)
	r.installValues(x.Intl, []valueDef{{d.dateTimeFormatName, ObjectValue(x.DateTimeFormatCtor), attrHidden}})
}

// intlDateTimeFormatCall implements Intl.DateTimeFormat(locales, options)
// called as a function: NewTarget is the constructor.
func intlDateTimeFormatCall(r *Realm, this Value, args []Value) (Value, error) {
	return intlDateTimeFormatConstruct(r, args, nil)
}

// intlDateTimeFormatConstruct implements new Intl.DateTimeFormat(locales,
// options).
func intlDateTimeFormatConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	x := r.intlIntr()
	if newTarget == nil {
		newTarget = x.DateTimeFormatCtor
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, x.DateTimeFormatCtor, x.DateTimeFormatPrototype)
	if err != nil {
		return Undefined(), err
	}
	f, err := r.intlNewDateTimeFormat(intlArg(args, 0), intlArg(args, 1), intlRequireAny, intlDefaultsDate)
	if err != nil {
		return Undefined(), err
	}
	r.markPrototype(proto)
	o := r.newObject(ClassIntlDateTimeFormat, r.rootShapeFor(proto))
	setExtData(o, kindIntlDateTimeFormat, f)
	return ObjectValue(o), nil
}

// intlIsType reports whether s matches the Unicode locale nonterminal type:
// alphanum{3,8} subtags separated by '-'.
func intlIsType(s string) bool {
	for s != "" {
		end := strings.IndexByte(s, '-')
		if end < 0 {
			end = len(s)
		}
		if end < 3 || end > 8 || !ltAllAlnum(s[:end]) {
			return false
		}
		if end == len(s) {
			return true
		}
		s = s[end+1:]
		if s == "" {
			return false
		}
	}
	return false
}

// intlLocaleHours holds the hour cycles of a found locale, from CLDR
// timeData of its language and region (the region from the likely subtags
// when it has none): its preferred hour letter, and the first 12-hour and
// first 24-hour ones of its allowed list ([[hourCycle12]] and
// [[hourCycle24]]: h11 for Japan, whose allowed list is "H K h").
type intlLocaleHours struct {
	hourChar, hour12, hour24 byte
	// region is the found locale's region, "" for none (two letters or
	// three digits).
	region string
}

var intlHoursCache sync.Map // found locale → intlLocaleHours

func intlHoursOf(found string) intlLocaleHours {
	if v, ok := intlHoursCache.Load(found); ok {
		return v.(intlLocaleHours)
	}
	h := intlLocaleHours{hourChar: 'H', hour12: 'h', hour24: 'H'}
	if t, ok := parseLangTag(found); ok {
		// The found locale's region, else its likely one: only a locale
		// without a region decodes the likely subtags.
		h.region = t.region()
		if t.region() == "" {
			t.maximize()
		}
		pref, allowed := intlTimeData(t.language(), t.region())
		switch pref {
		case "h", "K", "k":
			h.hourChar = pref[0]
		}
		h12, h24 := byte(0), byte(0)
		for a := range strings.FieldsSeq(allowed) {
			switch c := a[0]; {
			case (c == 'h' || c == 'K') && h12 == 0:
				h12 = c
			case (c == 'H' || c == 'k') && h24 == 0:
				h24 = c
			}
		}
		if h12 != 0 {
			h.hour12 = h12
		}
		if h24 != 0 {
			h.hour24 = h24
		}
		if h.hourChar == 'h' || h.hourChar == 'K' {
			h.hour12 = h.hourChar
		} else {
			h.hour24 = h.hourChar
		}
	}
	intlHoursCache.Store(found, h)
	return h
}

// intlNewDateTimeFormat implements CreateDateTimeFormat (ECMA-402 §11.1.2)
// without the object: the constructor's (required any, defaults date) and
// the toLocaleString methods' (Date.prototype.toLocaleString: any, all;
// toLocaleDateString: date, date; toLocaleTimeString: time, time).
func (r *Realm) intlNewDateTimeFormat(locales, options Value, required intlDateRequired, defaults intlDateDefaults) (*intlDateTimeFormat, error) {
	const ctor = "Intl.DateTimeFormat"
	a := intlNames()
	n := intlDateNames()
	requested, err := r.canonicalizeLocaleList(locales)
	if err != nil {
		return nil, err
	}
	opts, err := r.coerceOptionsToObject(options)
	if err != nil {
		return nil, err
	}
	if _, err := r.intlLocaleMatcher(opts, ctor); err != nil {
		return nil, err
	}
	calendar, err := r.intlTypeOption(opts, ctor, a.calendar)
	if err != nil {
		return nil, err
	}
	numbering, err := r.intlTypeOption(opts, ctor, a.numberingSystem)
	if err != nil {
		return nil, err
	}
	hour12v, err := r.intlOption(opts, n.hour12)
	if err != nil {
		return nil, err
	}
	hasHour12, hour12 := !hour12v.IsUndefined(), ToBoolean(hour12v)
	hcOpt, err := r.intlStringOption(opts, ctor, a.hourCycle, a.h11, a.h12, a.h23, a.h24)
	if err != nil {
		return nil, err
	}
	if hasHour12 {
		hcOpt = nil
	}
	res := r.intlResolve(requested, intlHasData, []intlRelevantKey{
		{key: "ca", option: calendar, values: func(string) []string { return []string{"gregory"} }},
		{key: "hc", option: hcOpt, values: func(string) []string { return []string{"", "h11", "h12", "h23", "h24"} }},
		{key: "nu", option: numbering, values: intlNumberingValues},
	})
	if hasHour12 && res.values[1] != "" {
		// The hour12 option sets the hc option to null, which drops the
		// keyword.
		res.values[1] = ""
		res.locale = intlDropKeyword(res.locale, "hc")
	}
	// The strings the formatter keeps are its own and charged; its data,
	// patterns included, are the locale's, shared by the process.
	f := &intlDateTimeFormat{locale: r.intlString(res.locale), numbering: r.intlString(res.values[2])}
	r.charge(int(unsafe.Sizeof(intlDateTimeFormat{})))
	f.dataIndex, _ = intlDataIndex(res.dataLocale)
	f.data = intlDate(f.dataIndex)
	if res.values[2] != "latn" {
		f.digits = intlDigits(res.values[2])
	}
	f.decimal = f.data.decimalOf(res.values[2])

	// The hour cycle (V8's): the locale's from timeData, the hc keyword of
	// the requested locale being the pattern generator's default.
	hours := intlHoursOf(res.dataLocale)
	f.hourChar = hours.hourChar
	copy(f.region[:], hours.region)
	kwHC := byte(0)
	for _, l := range requested {
		if intlLookupIn(l, intlHasData) != "" {
			if v, ok := intlKeyword(intlUnicodeExtension(l), "hc"); ok {
				kwHC = intlHourCycleChar(v)
			}
			break
		}
	}
	hcDefault := hours.hourChar
	if kwHC != 0 {
		f.hourChar = kwHC
	}
	hc := intlHourCycleChar(res.values[1])
	switch {
	case hasHour12 && hour12:
		hc = hours.hour12
	case hasHour12:
		hc = hours.hour24
	case hc == 0:
		hc = hcDefault
	}

	tz, err := r.intlOption(opts, n.timeZone)
	if err != nil {
		return nil, err
	}
	if tz.IsUndefined() {
		f.zone = intlZoneOfLocation(r.TimeZone())
	} else {
		s, err := r.ToString(tz)
		if err != nil {
			return nil, err
		}
		z, msg := intlResolveZone(s.GoString())
		if z == nil {
			return nil, r.RangeError("%s", msg)
		}
		f.zone = z
	}
	f.zoneID = r.intlString(f.zone.id)

	// The components (Table 16) and their skeleton.
	var skel []byte
	explicit, hasHour, dateFields, timeFields := false, false, false, false
	comp := func(name *String, values []*String, letters []string) (bool, error) {
		v, err := r.intlStringOption(opts, ctor, name, values...)
		if err != nil || v == nil {
			return false, err
		}
		for i, c := range values {
			if c == v {
				skel = append(skel, letters[i]...)
			}
		}
		explicit = true
		return true, nil
	}
	nls := []*String{n.narrow, n.long, n.short}
	nums := []*String{n.twoDigit, n.numeric}
	h := string(hc)
	for i, s := range [...]struct {
		name    *String
		values  []*String
		letters []string
	}{
		{n.weekday, nls, []string{"EEEEE", "EEEE", "EEE"}},
		{n.era, nls, []string{"GGGGG", "GGGG", "GGG"}},
		{n.year, nums, []string{"yy", "y"}},
		{n.month, []*String{n.twoDigit, n.numeric, n.narrow, n.long, n.short}, []string{"MM", "M", "MMMMM", "MMMM", "MMM"}},
		{n.day, nums, []string{"dd", "d"}},
		{n.dayPeriod, nls, []string{"BBBBB", "BBBB", "B"}},
		{n.hour, nums, []string{h + h, h}},
		{n.minute, nums, []string{"mm", "m"}},
		{n.second, nums, []string{"ss", "s"}},
	} {
		set, err := comp(s.name, s.values, s.letters)
		if err != nil {
			return nil, err
		}
		switch {
		case !set:
		case i == 0 || i >= 2 && i <= 4:
			dateFields = true
		case i >= 5:
			timeFields = true
			hasHour = hasHour || i == 6
		}
	}
	fsdv, err := r.intlOption(opts, n.fractionalSecondDigits)
	if err != nil {
		return nil, err
	}
	if !fsdv.IsUndefined() {
		fsd, err := r.ToNumber(fsdv)
		if err != nil {
			return nil, err
		}
		if !isFinite(fsd) || fsd < 1 || fsd > 3 {
			return nil, r.RangeError("fractionalSecondDigits value is out of range.")
		}
		for range int(math.Floor(fsd)) {
			skel = append(skel, 'S')
		}
		explicit, timeFields = true, true
	}
	if _, err := comp(n.timeZoneName, []*String{n.long, n.short, n.longOffset, n.shortOffset, n.longGeneric, n.shortGeneric},
		[]string{"zzzz", "z", "OOOO", "O", "vvvv", "v"}); err != nil {
		return nil, err
	}
	if _, err := r.intlStringOption(opts, ctor, n.formatMatcher, n.basic, a.bestFit); err != nil {
		return nil, err
	}
	styles := []*String{n.full, n.long, n.medium, n.short}
	ds, err := r.intlStringOption(opts, ctor, n.dateStyle, styles...)
	if err != nil {
		return nil, err
	}
	ts, err := r.intlStringOption(opts, ctor, n.timeStyle, styles...)
	if err != nil {
		return nil, err
	}
	for i, s := range styles {
		if ds == s {
			f.dateStyle = int8(i + 1)
		}
		if ts == s {
			f.timeStyle = int8(i + 1)
		}
	}

	if f.dateStyle != 0 || f.timeStyle != 0 {
		if explicit {
			return nil, r.TypeError("Invalid option : option")
		}
		if required == intlRequireDate && f.timeStyle != 0 {
			return nil, r.TypeError("Invalid option : timeStyle")
		}
		if required == intlRequireTime && f.dateStyle != 0 {
			return nil, r.TypeError("Invalid option : dateStyle")
		}
		if f.timeStyle != 0 {
			f.hourCycle = hc
		}
		key := intlPatternKey{kind: intlStylePattern, skeleton: string([]byte{byte(f.dateStyle), byte(f.timeStyle), kwHC}),
			decimal: f.decimal, hourChar: f.hourChar, hourCycle: f.hourCycle}
		f.pattern = f.data.cachedPattern(r, key, func() string { return f.stylePattern(kwHC) })
	} else {
		// needDefaults: no date component when required is date or any, no
		// time component when it is time or any.
		need := !(required != intlRequireTime && dateFields || required != intlRequireDate && timeFields)
		if need && (defaults == intlDefaultsDate || defaults == intlDefaultsAll) {
			skel = append(skel, "yMd"...)
		}
		if need && (defaults == intlDefaultsTime || defaults == intlDefaultsAll) {
			switch hc {
			case 'h':
				skel = append(skel, "hms"...)
			case 'K':
				skel = append(skel, "Kms"...)
			case 'k':
				skel = append(skel, "kms"...)
			default:
				skel = append(skel, "Hms"...)
			}
		}
		if hasHour {
			f.hourCycle = hc
		}
		key := intlPatternKey{kind: intlComponentsPattern, skeleton: string(skel), decimal: f.decimal, hourChar: f.hourChar, hourCycle: f.hourCycle}
		f.pattern = f.data.cachedPattern(r, key, func() string {
			return intlReplaceHourCycle(f.bestPattern(true, key.skeleton, dtMatchHourLength), f.hourCycle)
		})
	}
	f.items = f.data.compile(r, f.pattern)
	for _, it := range f.items {
		switch it.ch {
		case 'm':
			f.hasMinute = true
		case 's':
			f.hasSecond = true
		}
	}
	return f, nil
}

// intlTypeOption reads the calendar or numberingSystem option: undefined,
// or a string matching the type nonterminal (ECMA-402 §9.2.8 step 6.e).
func (r *Realm) intlTypeOption(options *Object, ctor string, name *String) (*String, error) {
	s, err := r.intlStringOption(options, ctor, name)
	if err != nil || s == nil {
		return s, err
	}
	if !intlIsType(s.GoString()) {
		return nil, r.RangeError("Invalid %s : %s", name.GoString(), s.GoString())
	}
	return s, nil
}

// intlDropKeyword removes a key's keyword from a locale whose Unicode
// extension ResolveLocale built (keywords only).
func intlDropKeyword(locale, key string) string {
	i := strings.Index(locale, "-u-")
	if i < 0 {
		return locale
	}
	base, ext := locale[:i], locale[i+3:]
	var kept []string
	subs := strings.Split(ext, "-")
	for j := 0; j < len(subs); {
		k := j + 1
		for k < len(subs) && len(subs[k]) != 2 {
			k++
		}
		if subs[j] != key {
			kept = append(kept, subs[j:k]...)
		}
		j = k
	}
	if len(kept) == 0 {
		return base
	}
	return base + "-u-" + strings.Join(kept, "-")
}

// intlPatternKey keys the pattern cache of a locale.
type intlPatternKey struct {
	kind      uint8 // intlBestPattern and the others
	skeleton  string
	decimal   string
	hourChar  byte
	hourCycle byte
	std       bool
	options   int8
}

// The kinds of pattern the locale caches: getBestPattern's, and a
// formatter's pattern of components or of styles (with its hour cycle).
const (
	intlBestPattern uint8 = iota
	intlComponentsPattern
	intlStylePattern
)

// cachedPattern returns the pattern of key from the locale's cache, making
// it with mk on a miss; once the caches' budget is spent, the pattern is
// the formatter's own and charged to r (nil for no charge).
func (d *intlDateData) cachedPattern(r *Realm, key intlPatternKey, mk func() string) string {
	if p, ok := d.patterns.Load(key); ok {
		return p.(string)
	}
	p := mk()
	if n := int(unsafe.Sizeof(key)) + len(key.skeleton) + len(key.decimal) + len(p); intlCacheTake(n) {
		if v, loaded := d.patterns.LoadOrStore(key, p); loaded {
			intlCacheRefund(n)
			p = v.(string)
		}
	} else if r != nil {
		r.charge(len(p))
	}
	return p
}

// bestPattern returns the pattern generator's best pattern of a skeleton
// for the formatter's hour letter and decimal separator.
func (f *intlDateTimeFormat) bestPattern(std bool, skeleton string, options int) string {
	return f.data.bestPatternFor(std, f.hourChar, f.decimal, skeleton, options)
}

// bestPatternFor returns the pattern generator's best pattern of a
// skeleton, from the locale's pattern map with or without the style
// patterns, for a default hour letter and a decimal separator; the
// results are cached by locale.
func (d *intlDateData) bestPatternFor(std bool, hourChar byte, decimal, skeleton string, options int) string {
	key := intlPatternKey{skeleton: skeleton, decimal: decimal, hourChar: hourChar, std: std, options: int8(options >> dtHour)}
	return d.cachedPattern(nil, key, func() string {
		g := &intlDTPG{m: d.patternMap(std), hourChar: hourChar, decimal: decimal}
		return g.bestPattern(skeleton, options)
	})
}

// stylePattern returns the pattern of the date and time styles, as V8's
// DateTimeStylePattern: ICU's style pattern (whose time part the pattern
// generator makes when the locale has an hc keyword), regenerated from its
// skeleton with the formatter's hour cycle when it has another one.
func (f *intlDateTimeFormat) stylePattern(kwHC byte) string {
	d := f.data
	ds, ts := int(f.dateStyle)-1, int(f.timeStyle)-1
	var time string
	if ts >= 0 {
		if kwHC != 0 {
			skel := [...]string{"jmmsszzzz", "jmmssz", "jmmss", "jmm"}[ts]
			time = d.bestPatternFor(false, kwHC, f.decimal, skel, 0)
		} else {
			time = d.timeFormats[ts]
		}
	}
	var p string
	switch {
	case ds >= 0 && ts >= 0:
		p = intlSimpleFormat(d.atTime[ds], time, d.dateFormats[ds])
	case ds >= 0:
		return d.dateFormats[ds]
	default:
		p = time
	}
	if f.hourCycle == intlHourCycleOf(p) {
		return p
	}
	skel := intlReplaceSkeletonHour(intlStaticSkeleton(p), f.hourCycle)
	return intlReplaceHourCycle(f.bestPattern(true, skel, dtMatchHourLength), f.hourCycle)
}

// intlHourCycleOf returns the hour letter of the first hour field of a
// pattern (V8's HourCycleFromPattern), 0 when it has none.
func intlHourCycleOf(p string) byte {
	inQuote := false
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\'':
			inQuote = !inQuote
		case 'K', 'h', 'H', 'k':
			if !inQuote {
				return c
			}
		}
	}
	return 0
}

// intlReplaceSkeletonHour is V8's ReplaceSkeleton: the skeleton without
// day periods and with its hour letters replaced.
func intlReplaceSkeletonHour(s string, hc byte) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 'a', 'b', 'B':
		case 'h', 'H', 'K', 'k':
			b = append(b, hc)
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// intlReplaceHourCycle is V8's ReplaceHourCycleInPattern: the hour letters
// outside quotes replaced by the hour cycle's (a space added between a day
// and an hour that follows it directly); unchanged for hour cycle 0.
func intlReplaceHourCycle(p string, hc byte) string {
	if hc == 0 || strings.IndexAny(p, "hHkK") < 0 {
		return p
	}
	b := make([]byte, 0, len(p)+1)
	replace := true
	last := byte(0)
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '\'':
			replace = !replace
			b = append(b, c)
		case 'H', 'h', 'K', 'k':
			if replace && last == 'd' {
				b = append(b, ' ')
			}
			if replace {
				b = append(b, hc)
			} else {
				b = append(b, c)
			}
		default:
			b = append(b, c)
		}
		last = c
	}
	return string(b)
}

// thisDateTimeFormat implements the receiver check of the prototype
// methods (UnwrapDateTimeFormat without the legacy fallback).
func thisDateTimeFormat(r *Realm, this Value, method string) (*intlDateTimeFormat, error) {
	return thisIntl[*intlDateTimeFormat](r, this, ClassIntlDateTimeFormat, method)
}

// intlDateValue implements the Number steps of ToDateTimeFormattable and
// FormatDateTime's TimeClip: undefined is now; NaN after TimeClip throws.
func (r *Realm) intlDateValue(v Value) (float64, error) {
	var tv float64
	if v.IsUndefined() {
		tv = r.nowTimeValue()
	} else {
		x, err := r.ToNumber(v)
		if err != nil {
			return 0, err
		}
		tv = x
	}
	tv = timeClip(tv)
	if tv != tv {
		return 0, r.RangeError("Invalid time value")
	}
	return tv, nil
}

// format formats a finite, time-clipped time value; it allocates only the
// result. As V8's FormatDateTime, it replaces the U+202F NARROW NO-BREAK
// SPACE of ICU 72 and later (before AM/PM in English) with a space;
// formatToParts and the ranges keep it.
func (f *intlDateTimeFormat) format(tv float64) *String {
	var buf [128]byte
	return intlUTF8String(intlNNBSPToSpace(f.appendFormat(buf[:0], f.items, tv, nil)))
}

// intlNNBSPToSpace replaces every U+202F of UTF-8 bytes with a space, in
// place.
func intlNNBSPToSpace(b []byte) []byte {
	j := 0
	for i := 0; i < len(b); i++ {
		if b[i] == 0xE2 && i+2 < len(b) && b[i+1] == 0x80 && b[i+2] == 0xAF {
			b[j] = ' '
			j++
			i += 2
			continue
		}
		b[j] = b[i]
		j++
	}
	return b[:j]
}

// intlDateTimeFormatFormatGetter implements the getter of
// Intl.DateTimeFormat.prototype.format: a function bound to the
// formatter, made once.
func intlDateTimeFormatFormatGetter(r *Realm, this Value, args []Value) (Value, error) {
	f, err := thisDateTimeFormat(r, this, "get Intl.DateTimeFormat.prototype.format")
	if err != nil {
		return Undefined(), err
	}
	if f.boundFormat == nil {
		f.boundFormat = r.NewNativeFunction(emptyString, 1, func(r *Realm, this Value, args []Value) (Value, error) {
			tv, err := r.intlDateValue(intlArg(args, 0))
			if err != nil {
				return Undefined(), err
			}
			return StringValue(r.intlCharged(f.format(tv))), nil
		})
	}
	return ObjectValue(f.boundFormat), nil
}

// intlDateTimeFormatFormatToParts implements
// Intl.DateTimeFormat.prototype.formatToParts(date).
func intlDateTimeFormatFormatToParts(r *Realm, this Value, args []Value) (Value, error) {
	f, err := thisDateTimeFormat(r, this, "Intl.DateTimeFormat.prototype.formatToParts")
	if err != nil {
		return Undefined(), err
	}
	tv, err := r.intlDateValue(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	var buf [128]byte
	var spanBuf [24]intlDateSpan
	spans := spanBuf[:0]
	out := f.appendFormat(buf[:0], f.items, tv, &spans)
	return ObjectValue(r.intlDateParts(out, spans, nil)), nil
}

// intlDateParts makes the array of formatToParts (and, with sources, of
// formatRangeToParts): the fields of spans and the literals between them.
func (r *Realm) intlDateParts(out []byte, spans []intlDateSpan, sources func(start, end int) *String) *Object {
	a := intlDateNames()
	s := string(out)
	shape := r.plainRoot.addProperty(r, StringKey(a.typ), attrDefault).addProperty(r, StringKey(a.value), attrDefault)
	if sources != nil {
		shape = shape.addProperty(r, StringKey(a.source), attrDefault)
	}
	items := make([]Value, 0, 2*len(spans)+1)
	add := func(typ *String, start, end int) {
		o := r.intlPartObject(shape, sources != nil)
		o.slots[0] = StringValue(typ)
		o.slots[1] = StringValue(r.intlPartString(s[start:end]))
		if sources != nil {
			o.slots[2] = StringValue(sources(start, end))
		}
		items = append(items, ObjectValue(o))
	}
	prev := 0
	for _, sp := range spans {
		if sp.start > prev {
			add(a.literal, prev, sp.start)
		}
		add(intlPartType(sp.ch), sp.start, sp.end)
		prev = sp.end
	}
	if prev < len(s) {
		add(a.literal, prev, len(s))
	}
	return r.NewArrayFromSlice(items)
}

// intlPartString returns the charged String of a UTF-8 substring of
// formatted output: a shared one for one ASCII character, else one
// sharing the bytes when they are ASCII.
func (r *Realm) intlPartString(s string) *String {
	if len(s) == 1 && s[0] < 0x80 {
		return singleCharStrings[s[0]]
	}
	if isASCII(s) {
		return r.intlCharged(asciiString(s))
	}
	return r.intlCharged(FromGoString(s))
}

// The part objects of formatToParts: two or three slots with the object.
type (
	intlPart2Object struct {
		obj   Object
		slots [2]Value
	}
	intlPart3Object struct {
		obj   Object
		slots [3]Value
	}
)

func (r *Realm) intlPartObject(shape *Shape, source bool) *Object {
	var o *Object
	if source {
		p := &intlPart3Object{}
		r.chargeObject(unsafe.Sizeof(intlPart3Object{}))
		o = &p.obj
		o.slots = p.slots[:]
	} else {
		p := &intlPart2Object{}
		r.chargeObject(unsafe.Sizeof(intlPart2Object{}))
		o = &p.obj
		o.slots = p.slots[:]
	}
	o.shape = shape
	o.class = ClassObject
	o.flags = flagExtensible
	return o
}

// intlDateTimeFormatResolvedOptions implements
// Intl.DateTimeFormat.prototype.resolvedOptions(); the components come from
// the pattern as V8 reads them.
func intlDateTimeFormatResolvedOptions(r *Realm, this Value, args []Value) (Value, error) {
	f, err := thisDateTimeFormat(r, this, "Intl.DateTimeFormat.prototype.resolvedOptions")
	if err != nil {
		return Undefined(), err
	}
	a := intlNames()
	n := intlDateNames()
	o := r.NewObjectCap(8)
	add := func(name *String, v Value) {
		o.addNamed(r, StringKey(name), propCell{value: v, attrs: attrDefault})
	}
	add(a.locale, StringValue(f.locale))
	add(a.calendar, StringValue(n.gregory))
	add(a.numberingSystem, StringValue(f.numbering))
	add(n.timeZone, StringValue(f.zoneID))
	if f.hourCycle != 0 {
		add(a.hourCycle, StringValue(intlHourCycleName(f.hourCycle)))
		add(n.hour12, Bool(f.hourCycle == 'K' || f.hourCycle == 'h'))
	}
	if f.dateStyle == 0 && f.timeStyle == 0 {
		p := f.pattern
		items := []struct {
			name  *String
			pairs []string // pattern, value index into vals
			vals  []*String
		}{
			{n.weekday, []string{"EEEEE", "EEEE", "EEE", "ccccc", "cccc", "ccc"}, []*String{n.narrow, n.long, n.short, n.narrow, n.long, n.short}},
			{n.era, []string{"GGGGG", "GGGG", "GGG"}, []*String{n.narrow, n.long, n.short}},
			{n.year, []string{"yy", "y"}, []*String{n.twoDigit, n.numeric}},
			{n.month, []string{"MMMMM", "MMMM", "MMM", "MM", "M", "LLLLL", "LLLL", "LLL", "LL", "L"},
				[]*String{n.narrow, n.long, n.short, n.twoDigit, n.numeric, n.narrow, n.long, n.short, n.twoDigit, n.numeric}},
			{n.day, []string{"dd", "d"}, []*String{n.twoDigit, n.numeric}},
			{n.dayPeriod, []string{"BBBBB", "bbbbb", "BBBB", "bbbb", "B", "b"}, []*String{n.narrow, n.narrow, n.long, n.long, n.short, n.short}},
			{n.hour, []string{"HH", "H", "hh", "h", "kk", "k", "KK", "K"}, []*String{n.twoDigit, n.numeric, n.twoDigit, n.numeric, n.twoDigit, n.numeric, n.twoDigit, n.numeric}},
			{n.minute, []string{"mm", "m"}, []*String{n.twoDigit, n.numeric}},
			{n.second, []string{"ss", "s"}, []*String{n.twoDigit, n.numeric}},
			{n.timeZoneName, []string{"zzzz", "z", "OOOO", "O", "vvvv", "v"}, []*String{n.long, n.short, n.longOffset, n.shortOffset, n.longGeneric, n.shortGeneric}},
		}
		for _, it := range items {
			if it.name == n.timeZoneName {
				if fsd := min(strings.Count(p, "S"), 3); fsd > 0 {
					add(n.fractionalSecondDigits, IntValue(fsd))
				}
			}
			for i, pat := range it.pairs {
				if strings.Contains(p, pat) {
					add(it.name, StringValue(it.vals[i]))
					break
				}
			}
		}
	}
	styles := []*String{n.full, n.long, n.medium, n.short}
	if f.dateStyle != 0 {
		add(n.dateStyle, StringValue(styles[f.dateStyle-1]))
	}
	if f.timeStyle != 0 {
		add(n.timeStyle, StringValue(styles[f.timeStyle-1]))
	}
	return ObjectValue(o), nil
}

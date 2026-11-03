package engine

// Ported from ICU's SimpleDateFormat (icu4c/source/i18n/smpdtfmt.cpp;
// Unicode License v3; see THIRD_PARTY_NOTICES).

// Formatting with a date pattern, as ICU's SimpleDateFormat does: the
// pattern compiles once into literal runs and fields, and a time value
// formats into UTF-8 bytes, with the span of each field for formatToParts.
// The calendar is the proleptic Gregorian one (V8 moves ICU's Gregorian
// change to the start of ECMAScript time); a year is its era's year.

import "unicode/utf8"

// intlDateItem is a compiled pattern item: a literal (ch 0) or a field of
// a pattern letter repeated count times.
type intlDateItem struct {
	lit   string
	ch    byte
	count int8
}

// intlCompilePattern splits a pattern into literals and fields
// (SimpleDateFormat::_format's scan): runs of an ASCII letter are fields,
// quotes delimit literals, two quotes are one.
func intlCompilePattern(p string) []intlDateItem {
	var items []intlDateItem
	var lit []byte
	flush := func() {
		if len(lit) > 0 {
			items = append(items, intlDateItem{lit: string(lit)})
			lit = lit[:0]
		}
	}
	inQuote := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\'':
			if i+1 < len(p) && p[i+1] == '\'' {
				lit = append(lit, '\'')
				i++
			} else {
				inQuote = !inQuote
			}
		case !inQuote && dtIsLetter(c):
			n := 1
			for i+1 < len(p) && p[i+1] == c {
				n++
				i++
			}
			flush()
			items = append(items, intlDateItem{ch: c, count: int8(min(n, 127))})
		default:
			lit = append(lit, c)
		}
	}
	flush()
	return items
}

// intlPartType is the formatToParts type of a pattern letter, 0 for a
// literal.
func intlPartType(ch byte) *String {
	a := intlDateNames()
	switch ch {
	case 'G':
		return a.era
	case 'y', 'Y', 'u', 'U', 'r':
		return a.year
	case 'M', 'L':
		return a.month
	case 'd':
		return a.day
	case 'h', 'H', 'k', 'K':
		return a.hour
	case 'm':
		return a.minute
	case 's':
		return a.second
	case 'E', 'e', 'c':
		return a.weekday
	case 'a', 'b', 'B':
		return a.dayPeriod
	case 'z', 'Z', 'v', 'V', 'O', 'X', 'x':
		return a.timeZoneName
	case 'S':
		return a.fractionalSecond
	}
	return a.literal
}

// intlDateSpan is the span of a field in formatted bytes.
type intlDateSpan struct {
	ch         byte
	start, end int
}

// appendFormat appends the formatting of a finite, time-clipped time value
// to b; spans, when not nil, receives the span of every non-empty field.
func (f *intlDateTimeFormat) appendFormat(b []byte, items []intlDateItem, tv float64, spans *[]intlDateSpan) []byte {
	_, off, _ := f.zone.at(tv)
	t := splitTime(int64(tv) + int64(off)*1000)
	for i := range items {
		it := &items[i]
		if it.ch == 0 {
			b = append(b, it.lit...)
			continue
		}
		start := len(b)
		b = f.appendField(b, it.ch, int(it.count), &t, tv)
		if spans != nil && len(b) > start {
			*spans = append(*spans, intlDateSpan{it.ch, start, len(b)})
		}
	}
	return b
}

// appendField is SimpleDateFormat::subFormat for the Gregorian calendar.
func (f *intlDateTimeFormat) appendField(b []byte, ch byte, count int, t *dateFields, tv float64) []byte {
	d := f.data
	width := func() int { // abbreviated, wide, narrow
		switch count {
		case 4:
			return 1
		case 5:
			return 2
		}
		return 0
	}
	switch ch {
	case 'G':
		era := 1
		if t.year <= 0 {
			era = 0
		}
		return append(b, d.eras[width()][era]...)
	case 'y':
		y := t.year
		if y <= 0 {
			y = 1 - y
		}
		if count == 2 {
			return f.appendNumber(b, y%100, 2)
		}
		return f.appendNumber(b, y, count)
	case 'M', 'L':
		ctx := 0
		if ch == 'L' {
			ctx = 3
		}
		switch count {
		case 5, 4, 3:
			w := 0
			if count == 4 {
				w = 1
			} else if count == 5 {
				w = 2
			}
			return append(b, d.months[ctx+w][t.month]...)
		}
		return f.appendNumber(b, int64(t.month)+1, count)
	case 'd':
		return f.appendNumber(b, int64(t.day), count)
	case 'E', 'e', 'c':
		if ch != 'E' && count < 3 {
			// The local day of the week; no option asks for it.
			return f.appendNumber(b, int64(t.weekday)+1, count)
		}
		ctx := 0
		if ch == 'c' {
			ctx = 3
		}
		return append(b, d.days[ctx+width()][t.weekday]...)
	case 'a':
		return append(b, d.ampm[width()][t.hour/12]...)
	case 'b':
		if t.hour == 12 && (!f.hasMinute || t.minute == 0) && (!f.hasSecond || t.second == 0) {
			if s := d.periods[f.periodWidth(count)][1]; s != "" {
				return append(b, s...)
			}
		}
		return append(b, d.ampm[width()][t.hour/12]...)
	case 'B':
		return f.appendFlexiblePeriod(b, count, t)
	case 'h':
		h := t.hour % 12
		if h == 0 {
			h = 12
		}
		return f.appendNumber(b, int64(h), count)
	case 'H':
		return f.appendNumber(b, int64(t.hour), count)
	case 'K':
		return f.appendNumber(b, int64(t.hour%12), count)
	case 'k':
		h := t.hour
		if h == 0 {
			h = 24
		}
		return f.appendNumber(b, int64(h), count)
	case 'm':
		return f.appendNumber(b, int64(t.minute), count)
	case 's':
		return f.appendNumber(b, int64(t.second), count)
	case 'S':
		v := int64(t.ms)
		switch count {
		case 1:
			v /= 100
		case 2:
			v /= 10
		}
		b = f.appendNumber(b, v, min(count, 3))
		if count > 3 {
			b = f.appendNumber(b, 0, count-3)
		}
		return b
	case 'z':
		style := intlZoneShort
		if count >= 4 {
			style = intlZoneLong
		}
		return f.zone.appendNameFor(b, f.dataIndex, f.regionName(), style, tv)
	case 'O':
		style := intlZoneShortOffset
		if count == 4 {
			style = intlZoneLongOffset
		} else if count != 1 {
			return b
		}
		return f.zone.appendNameFor(b, f.dataIndex, f.regionName(), style, tv)
	case 'v':
		style := intlZoneShortGeneric
		if count == 4 {
			style = intlZoneLongGeneric
		} else if count != 1 {
			return b
		}
		return f.zone.appendNameFor(b, f.dataIndex, f.regionName(), style, tv)
	}
	// Letters no Gregorian pattern of the data has.
	return b
}

// periodWidth returns the width of the day period names for a count
// (abbreviated up to 3, wide for 4 and more than 5, narrow for 5).
func (f *intlDateTimeFormat) periodWidth(count int) int {
	switch {
	case count <= 3:
		return 0
	case count == 5:
		return 2
	}
	return 1
}

// appendFlexiblePeriod formats B: the day period of the locale's rules
// for the time as displayed, midnight and noon only when the minutes and
// seconds the pattern shows are zero; am and pm and missing names fall
// back to the am/pm marker.
func (f *intlDateTimeFormat) appendFlexiblePeriod(b []byte, count int, t *dateFields) []byte {
	d := f.data
	minute, second := 0, 0
	if f.hasMinute {
		minute = t.minute
	}
	if f.hasSecond {
		second = t.second
	}
	period := d.periodOf(t.hour)
	switch {
	case t.hour == 0 && minute == 0 && second == 0 && d.hasMidnight():
		period = 0
	case t.hour == 12 && minute == 0 && second == 0 && d.hasNoon():
		period = 1
	}
	w := f.periodWidth(count)
	name := ""
	if period < 10 && period != 0 {
		name = d.periods[w][period]
	}
	if name == "" && (period == 0 || period == 1) {
		period = d.periodOf(t.hour)
		if period < 10 {
			name = d.periods[w][period]
		}
	}
	if period >= 10 || name == "" {
		aw := 0
		switch count {
		case 4:
			aw = 1
		case 5:
			aw = 2
		}
		return append(b, d.ampm[aw][t.hour/12]...)
	}
	return append(b, name...)
}

// appendNumber appends v zero-padded to minDigits (at most 10 digits, as
// ICU's maxIntCount) in the formatter's digits.
func (f *intlDateTimeFormat) appendNumber(b []byte, v int64, minDigits int) []byte {
	v %= 10_000_000_000
	start := len(b)
	b = appendPadded(b, v, minDigits)
	if f.digits == nil {
		return b
	}
	var tmp [16]byte
	digits := append(tmp[:0], b[start:]...)
	b = b[:start]
	for _, c := range digits {
		b = utf8.AppendRune(b, f.digits[c-'0'])
	}
	return b
}

// intlUTF8String returns the String of UTF-8 bytes: an ASCII String
// sharing its header's allocation (asciiStringCopy), else UTF-16 code
// units sharing theirs (newUTF16Buf).
func intlUTF8String(b []byte) *String {
	n := 0 // UTF-16 units
	ascii := true
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			n++
			continue
		}
		ascii = false
		r, size := utf8.DecodeRune(b[i:])
		i += size
		n++
		if r >= 0x10000 {
			n++
		}
	}
	if ascii {
		return asciiStringCopy(b)
	}
	s, u := newUTF16Buf(n)
	j := 0
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			u[j] = uint16(c)
			i++
			j++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if r >= 0x10000 {
			r -= 0x10000
			u[j], u[j+1] = uint16(0xD800+r>>10), uint16(0xDC00+r&0x3FF)
			j += 2
			continue
		}
		u[j] = uint16(r)
		j++
	}
	return s
}

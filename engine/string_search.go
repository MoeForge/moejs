package engine

import "strings"

// The direct scans of flatIndexOf and lastIndexOfString take quadratic time
// on a long pattern whose first units keep matching, as 一一…一丁 does in a
// text of 一. This file has the linear search they hand over to: Two-Way
// (Crochemore and Perrin, 1991), which glibc's strstr and Rust's str::find
// use. It needs no table, only two integers of the pattern: the pattern is
// cut at a critical position, where its local period is its period; each
// alignment compares the right part forwards, then the left part backwards,
// and shifts by what the comparisons proved, so each unit of the text is
// compared at most twice. The loops index slices of code units, the units
// of a UTF-16 text and the bytes or units of the pattern, which the type
// parameter stands for.

// directScanSwitch reports whether a direct scan hands over to
// searchFlatUnits: the pattern is long enough for the preprocessing to
// matter, the units its partial matches compared (hits) outweigh twice the
// positions it scanned plus one pattern, so it is heading for quadratic
// time, and enough positions remain for the linear search to pay off.
// Two-Way reads a unit at about twice the cost the scan reads one, so a
// scan whose partial matches stay within that ratio is the better search
// and keeps going. A scan costs its positions plus its hits, so one that
// hands over has spent at most three times its positions plus a pattern,
// and the whole search stays linear in the text and the pattern.
func directScanSwitch(m, hits, scanned, remaining int) bool {
	return m >= 16 && hits > 2*scanned+m && remaining > 16
}

// searchFlatUnits is the linear search of the flat text for the flat,
// non-empty pattern: forward, the first occurrence at or after from;
// reverse, the last occurrence at or before from, where from+pattern.Len()
// <= text.Len(). It returns -1 when there is none. The reverse search scans
// text[:from+m] forwards for every occurrence and keeps the last, which
// Two-Way does in the same linear time.
func searchFlatUnits(text, pattern *String, from int, reverse bool) int {
	m := int(pattern.n)
	if text.kind == strASCII {
		if pattern.kind != strASCII {
			return -1 // a UTF-16 pattern has a unit no ASCII text has
		}
		if reverse {
			return strings.LastIndex(text.s[:from+m], pattern.s)
		}
		if i := strings.Index(text.s[from:], pattern.s); i >= 0 {
			return from + i
		}
		return -1
	}
	u := text.units()
	if pattern.kind == strASCII {
		if reverse {
			return twoWay(u[:from+m], asciiBytes(pattern.s), true)
		}
		return searchFrom(u, asciiBytes(pattern.s), from)
	}
	if reverse {
		return twoWay(u[:from+m], pattern.units(), true)
	}
	return searchFrom(u, pattern.units(), from)
}

// searchFrom is the forward twoWay on text[from:], with its result in text.
func searchFrom[P codeUnit](text []uint16, pattern []P, from int) int {
	if i := twoWay(text[from:], pattern, false); i >= 0 {
		return from + i
	}
	return -1
}

// twoWay returns the index in text of the first occurrence of pattern, or
// of the last one when last is set, or -1.
func twoWay[P codeUnit](text []uint16, pattern []P, last bool) int {
	n, m := len(text), len(pattern)
	// The critical factorization: cut is the later of the two maximal
	// suffixes' positions, period the period of that suffix.
	cut, period := maximalSuffix(pattern, false)
	if c, p := maximalSuffix(pattern, true); c > cut {
		cut, period = c, p
	}
	// When the left part repeats at that period, the period is the whole
	// pattern's: a shift by it keeps what the right part matched past the
	// shift (remember). Otherwise the pattern's period exceeds both parts,
	// so a shift one past the longer part is safe, and nothing is kept.
	remember := m - period
	for i := range cut {
		if pattern[i] != pattern[i+period] {
			period = max(cut, m-cut) + 1
			remember = 0
			break
		}
	}
	found := -1
	memory := 0
	for pos := 0; pos <= n-m; {
		t := text[pos : pos+m]
		// The right part, forwards. A mismatch at i rules out every
		// alignment up to the one just past it.
		i := max(cut, memory)
		for i < m && uint16(pattern[i]) == t[i] {
			i++
		}
		if i < m {
			pos += i - cut + 1
			memory = 0
			continue
		}
		// The left part, backwards, down to what the previous alignment
		// proved after a periodic shift.
		i = cut
		for i > memory && uint16(pattern[i-1]) == t[i-1] {
			i--
		}
		if i <= memory {
			if !last {
				return pos
			}
			found = pos
		}
		pos += period
		memory = remember
	}
	return found
}

// maximalSuffix returns the position and the period of the maximal suffix
// of p under the order of its units, or under the inverted order when
// invert is set. The later of the two positions is a critical position.
func maximalSuffix[P codeUnit](p []P, invert bool) (int, int) {
	left, right, matched, period := -1, 0, 1, 1
	for right+matched < len(p) {
		a, b := p[right+matched], p[left+matched]
		switch {
		case a == b:
			if matched == period {
				right += period
				matched = 1
			} else {
				matched++
			}
		case (a < b) != invert:
			right += matched
			matched = 1
			period = right - left
		default:
			left = right
			right++
			matched, period = 1, 1
		}
	}
	return left + 1, period
}

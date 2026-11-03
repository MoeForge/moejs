package engine

type unitSearchView struct {
	s            *String
	offset, step int
}

func (v unitSearchView) at(i int) uint16 { return v.s.at(v.offset + i*v.step) }

func searchFlatUnits(text, pattern *String, from int, reverse bool) int {

	n, m := text.Len()-from, pattern.Len()
	t := unitSearchView{text, from, 1}
	p := unitSearchView{pattern, 0, 1}
	
	if reverse {
		n = from + m
		t.offset, t.step = n-1, -1
		p.offset, p.step = m-1, -1
	}
	
	i := searchUnitViews(t, p, n, m)
	if i < 0 {
		return -1
	}
	if reverse {
		return from - i
	}
	
	return from + i
}

func searchUnitViews(text, pattern unitSearchView, n, m int) int {

	cut, period := maximalUnitSuffix(pattern, m, false)
	if c, p := maximalUnitSuffix(pattern, m, true); c > cut {
		cut, period = c, p
	}
	// The right half's period extends across the cut exactly when this
	// prefix repeats at that period. Otherwise a longer shift is safe.
	remember := m - period

	for i := 0; i < cut; i++ {
		if pattern.at(i) != pattern.at(i+period) {
			period = max(cut-1, m-cut) + 1
			remember = 0
			break
		}
	}

	memory := 0
	for pos := 0; pos <= n-m; {

		// Match the right half first. A mismatch skips every alignment
		// up to and including the part just matched after the cut.
		i := max(cut, memory)
		for i < m && pattern.at(i) == text.at(pos+i) {
			i++
		}

		if i < m {
			pos += i - cut + 1
			memory = 0
			continue
		}

		// Then match the left half backwards. After a periodic shift,
		// the overlap was already compared at the previous alignment.
		i = cut
		for i > memory && pattern.at(i-1) == text.at(pos+i-1) {
			i--
		}
		if i <= memory {
			return pos
		}
		pos += period
		memory = remember
	}

	return -1
}

func maximalUnitSuffix(p unitSearchView, m int, invert bool) (int, int) {

	left, right, matched, period := -1, 0, 1, 1

	for right+matched < m {
		a, b := p.at(right+matched), p.at(left+matched)

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

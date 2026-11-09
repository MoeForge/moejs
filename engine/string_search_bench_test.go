package engine

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkStringSearch(b *testing.B) {
	for _, tc := range []struct {
		name, text, pattern string
	}{
		{"ascii-short", strings.Repeat("the quick brown fox ", 100), "fox"},
		{"utf16-short", strings.Repeat("\u4e00abcdef", 100), "def"},
		{"utf16-long", strings.Repeat("\u4e00abcdef", 1000), "\u4e00abcdef\u4e00abcdef"},
		{"mixed-long", "\u4e00" + strings.Repeat("abcdef", 1000), strings.Repeat("abcdef", 4)},
		{"prefix-miss", strings.Repeat("\u4e00", 32<<10), strings.Repeat("\u4e00", 1023) + "\u4e01"},
		{"suffix-miss", strings.Repeat("\u4e00", 32<<10), "\u4e01" + strings.Repeat("\u4e00", 1023)},
		{"periodic-miss", strings.Repeat("\u4e00\u4e01", 16<<10), "\u4e02" + strings.Repeat("\u4e00\u4e01", 511) + "\u4e00"},
		// The first unit is common and the rest fails at once: a direct
		// scan is linear here, and keeps the search.
		{"common-first-miss", "\u4e00" + strings.Repeat("ab", 20000), "ac" + strings.Repeat("x", 20)},
		// Words, with a long pattern that fails on its last unit.
		{"sentence-miss", "\u4e00" + strings.Repeat("the cat sat on the mat. ", 2000), "the cat sat on the mat! "},
		{"rare-first-miss", "\u4e00" + strings.Repeat("the cat sat on the mat. ", 2000), "Qthe cat sat on the mat."},
		// A CJK text whose period shares a long prefix with the pattern.
		{"cjk-prefix-miss", strings.Repeat("\u6211\u4eec\u7684", 5000), strings.Repeat("\u6211\u4eec\u7684", 5) + "\u6211\u4eec\u4e0d"},
	} {
		for _, reverse := range []bool{false, true} {
			direction := "forward"
			if reverse {
				direction = "reverse"
			}
			b.Run(tc.name+"/"+direction, func(b *testing.B) {
				s, p := FromGoString(tc.text), FromGoString(tc.pattern)
				want := referenceSearch(s.UTF16(), p.UTF16(), 0, false)
				if reverse {
					want = referenceSearch(s.UTF16(), p.UTF16(), s.Len(), true)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					got := -1
					if reverse {
						got = lastIndexOfString(s, p, s.Len())
					} else {
						got = s.IndexOf(p, 0)
					}
					if got != want {
						b.Fatalf("got %d, want %d", got, want)
					}
				}
			})
		}
	}
}

func BenchmarkStringSearchScaling(b *testing.B) {
	for _, n := range []int{4096, 16384, 65536} {
		for _, m := range []int{16, 256, 1024} {
			b.Run(fmt.Sprintf("text=%d/pattern=%d", n, m), func(b *testing.B) {
				s := FromGoString(strings.Repeat("\u4e00", n))
				p := FromGoString(strings.Repeat("\u4e00", m-1) + "x")
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if got := s.IndexOf(p, 0); got != -1 {
						b.Fatalf("got %d, want -1", got)
					}
				}
			})
		}
	}
}

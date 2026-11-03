package engine

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
)

func referenceSearch(text, pattern []uint16, from int, reverse bool) int {
	if reverse {
		for i := min(from, len(text)-len(pattern)); i >= 0; i-- {
			if slices.Equal(text[i:i+len(pattern)], pattern) {
				return i
			}
		}
		return -1
	}
	from = max(from, 0)
	if len(pattern) == 0 {
		return min(from, len(text))
	}
	for i := from; i <= len(text)-len(pattern); i++ {
		if slices.Equal(text[i:i+len(pattern)], pattern) {
			return i
		}
	}
	return -1
}

func checkStringSearch(t *testing.T, s, p *String, offsets ...int) {
	t.Helper()
	u, v := s.UTF16(), p.UTF16()
	for _, reverse := range []bool{false, true} {
		for _, from := range offsets {
			got := s.IndexOf(p, from)
			if reverse {
				got = lastIndexOfString(s, p, from)
			}
			if want := referenceSearch(u, v, from, reverse); got != want {
				t.Fatalf("text=%x pattern=%x reverse=%v from=%d: got %d, want %d", u, v, reverse, from, got, want)
			}
			// Also exercise Two-Way directly: the adaptive entry points
			// may satisfy a case before they need the linear fallback.
			start := max(from, 0)
			if reverse {
				start = min(from, len(u)-len(v))
			}
			if s.kind != strASCII && len(v) > 0 && start >= 0 && start <= len(u)-len(v) {
				flat, sub := FromUTF16(slices.Clone(u)), FromUTF16(slices.Clone(v))
				want := referenceSearch(u, v, start, reverse)
				if got := searchFlatUnits(flat, sub, start, reverse); got != want {
					t.Fatalf("Two-Way text=%x pattern=%x reverse=%v from=%d: got %d, want %d", u, v, reverse, start, got, want)
				}
			}
		}
	}
}

func TestStringSearchBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, text, pattern string
	}{
		{"empty", "", ""},
		{"empty-text", "", "abc"},
		{"empty-pattern", "\u4e00abc", ""},
		{"ascii", "abcabc", "abc"},
		{"impossible-encoding", "abcabc", "\u4e00"},
		{"longer-pattern", "\u4e00abc", "\u4e00abcabc"},
		{"overlap", "\u4e00\u4e00\u4e00", "\u4e00\u4e00"},
		{"mixed", "ab\u4e00abcabc", "abc"},
		{"pair", "a\U0001f600b\U0001f600", "\U0001f600"},
		{"nul", "\u4e00\x00abc\x00", "\x00"},
		{"equal-long", strings.Repeat("\u4e00a", 20), strings.Repeat("\u4e00a", 20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := FromGoString(tc.text), FromGoString(tc.pattern)
			for from := -2; from <= s.Len()+2; from++ {
				checkStringSearch(t, s, p, from)
			}
			checkStringSearch(t, s, p, int(^uint(0)>>1))
		})
	}
}

func TestStringSearchLongPatterns(t *testing.T) {
	for _, pattern := range []string{
		strings.Repeat("\u4e00", 31) + "\u4e01",
		"\u4e01" + strings.Repeat("\u4e00", 31),
		strings.Repeat("\u4e00\u4e01", 16),
		strings.Repeat("\u4e00ab", 11),
		strings.Repeat("ab", 16),
		"abcdefghijklmnopq",
		strings.Repeat("\U0001f600", 16),
	} {
		for _, text := range []string{
			strings.Repeat("\u4e00", 256),
			strings.Repeat("\u4e00\u4e01", 128),
			"\u4e02" + strings.Repeat("ab", 128),
			strings.Repeat("\u4e00", 80) + pattern + "x" + pattern + "\u4e03",
		} {
			s, p := FromGoString(text), FromGoString(pattern)
			for from := -1; from <= s.Len()+1; from++ {
				checkStringSearch(t, s, p, from)
			}
		}
	}
}

// Searches operate on code units, including half a surrogate pair. Neither
// the matching core nor the reverse view may decode them into code points.
func TestStringSearchSurrogates(t *testing.T) {
	u := slices.Repeat([]uint16{0xd800, 0xdc00, 'a', 0xdfff, 0, 0xd800}, 20)
	for _, v := range [][]uint16{
		{0xd800}, {0xdc00}, {0xdfff}, {0xdc00, 'a'}, {0, 0xd800},
		u[1:33], u[3:35], slices.Repeat([]uint16{0xd800}, 20),
	} {
		s, p := FromUTF16(slices.Clone(u)), FromUTF16(slices.Clone(v))
		for from := -1; from <= len(u)+1; from++ {
			checkStringSearch(t, s, p, from)
		}
	}
}

func TestStringSearchDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 3))
	alphabets := [][]uint16{{'a', 'b'}, {0x4e00, 0x4e01}, {'a', 0x4e00, 0xd800, 0xdc00, 0}}
	for round := range 4000 {
		alphabet := alphabets[round%len(alphabets)]
		u := make([]uint16, rng.IntN(256))
		v := make([]uint16, rng.IntN(96))
		for _, dst := range [][]uint16{u, v} {
			for i := range dst {
				dst[i] = alphabet[rng.IntN(len(alphabet))]
				if round%7 == 0 {
					dst[i] = uint16(rng.Uint32())
				}
			}
		}
		if round%2 == 0 && len(v) <= len(u) {
			at := rng.IntN(len(u) - len(v) + 1)
			copy(v, u[at:])
			if round%4 == 0 && len(v) > 0 {
				v[rng.IntN(len(v))] ^= 1
			}
		}
		s, p := FromUTF16(u), FromUTF16(v)
		checkStringSearch(t, s, p, -1, 0, len(u)/2, len(u), len(u)+1, rng.IntN(len(u)+1))
	}
}

func TestStringSearchRopes(t *testing.T) {
	for _, suffix := range []string{"a", "\u4e00", "\U0001f600"} {
		t.Run(fmt.Sprintf("%x", suffix), func(t *testing.T) {
			part := strings.Repeat(suffix, 40)
			s := ropeOf(FromGoString(part+"x"), FromGoString(part+"y"))
			p := ropeOf(FromGoString(part), FromGoString("x"+part))
			if s.kind != strRope || p.kind != strRope {
				t.Fatal("expected rope inputs")
			}
			// Exercise the first flatten, then the published flat view.
			if got := s.IndexOf(p, 0); got != 0 {
				t.Fatalf("first rope search: got %d, want 0", got)
			}
			checkStringSearch(t, s, p, -1, 0, 1, s.Len()/2, s.Len())
			flat := FromGoString(part + "x" + part)
			checkStringSearch(t, s, flat, 0, 1)
			checkStringSearch(t, FromGoString(part+"x"+part+"y"), p, 0, 1)
			if got := testing.AllocsPerRun(50, func() {
				s.IndexOf(p, 0)
				lastIndexOfString(s, p, s.Len())
			}); got != 0 {
				t.Fatalf("published rope searches allocated %g times", got)
			}
		})
	}
}

func TestStringSearchNoAlloc(t *testing.T) {
	s := FromGoString(strings.Repeat("\u4e00ab", 1000))
	for _, pattern := range []string{"a", "\u4e00", strings.Repeat("\u4e00ab", 10), strings.Repeat("ab", 100)} {
		p := FromGoString(pattern)
		if got := testing.AllocsPerRun(50, func() {
			s.IndexOf(p, 1)
			lastIndexOfString(s, p, s.Len()-1)
		}); got != 0 {
			t.Fatalf("pattern length %d: allocated %g times", p.Len(), got)
		}
	}
}

func TestStringSearchConcurrent(t *testing.T) {
	part := FromGoString(strings.Repeat("\u4e00", 2048))
	s := ropeOf(part, part)
	p := ropeOf(FromGoString(strings.Repeat("\u4e00", 63)), FromGoString("x"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if got := s.IndexOf(p, 0); got != -1 {
					t.Errorf("concurrent forward search: got %d, want -1", got)
					return
				}
				if got := lastIndexOfString(s, p, s.Len()); got != -1 {
					t.Errorf("concurrent reverse search: got %d, want -1", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestStringSearchBuiltins(t *testing.T) {
	const setup = `const p = "\u4e00".repeat(31) + "x";
const s = "\u4e00".repeat(200) + "x";`
	runProtoCases(t, []protoCase{
		{"long-index", setup + `return [s.indexOf(p), s.indexOf(p, 170), s.indexOf(p, -1)].join()`, "169,-1,169"},
		{"long-last-index", setup + `return [s.lastIndexOf(p), s.lastIndexOf(p, 168), s.lastIndexOf(p, Infinity)].join()`, "169,-1,169"},
		{"long-includes", setup + `return [s.includes(p), s.includes(p, 170)].join()`, "true,false"},
		{"long-split", setup + `return s.split(p).map(v => v.length).join()`, "169,0"},
		{"long-replace", setup + `return [s.replace(p, "z").length, (s + s).replaceAll(p, "z").length].join()`, "170,340"},
		{"long-replace-callback", setup + `let at = []; (s+s).replaceAll(p, (v, i) => {at.push(i); return v}); return at.join()`, "169,370"},
		{"surrogate-index", `const s = "\ud800\udc00".repeat(100); return [s.indexOf("\udc00"), s.lastIndexOf("\ud800"), s.indexOf("\udc00", 2)].join()`, "1,198,3"},
		{"offset-conversion", setup + `let n = 0; return [s.indexOf(p, {valueOf(){n++; return 169}}), n].join()`, "169,1"},
	})
}

func TestStringSearchHostAndJSON(t *testing.T) {
	f := evalModule(t, `export function search(s, p) {
  return [s.indexOf(p), s.lastIndexOf(p), s.includes(p),
          JSON.parse(JSON.stringify(s)).indexOf(p)].join();
}`)
	for _, unit := range []string{"a", "\u4e00", "\U0001f600"} {
		part := strings.Repeat(unit, 200) + "x"
		pattern := strings.Repeat(unit, 31) + "x"
		first := FromGoString(part).Len() - FromGoString(pattern).Len()
		last := first + FromGoString(part).Len()
		want := fmt.Sprintf("%d,%d,true,%d", first, last, first)
		if got := f.call("search", part+part, pattern); got != want {
			t.Fatalf("unit %x: got %v, want %s", unit, got, want)
		}
	}
}

func FuzzStringSearch(f *testing.F) {
	f.Add([]byte("ababa"), []byte("aba"), 0)
	f.Add([]byte{0, 0xd8, 0, 0xdc}, []byte{0, 0xdc}, 1)
	f.Add([]byte(strings.Repeat("ab", 80)), []byte(strings.Repeat("ab", 16)), 4)
	f.Fuzz(func(t *testing.T, text, pattern []byte, from int) {
		units := func(b []byte) []uint16 {
			b = b[:min(len(b), 1024)]
			u := make([]uint16, (len(b)+1)/2)
			for i, v := range b {
				u[i/2] |= uint16(v) << (8 * (i % 2))
			}
			return u
		}
		s, p := FromUTF16(units(text)), FromUTF16(units(pattern))
		checkStringSearch(t, s, p, from)
	})
}

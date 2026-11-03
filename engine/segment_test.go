package engine

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

var segNames = [...]string{"grapheme", "word", "sentence"}

// segTextOf returns the segText of the code units u: ascii when they are
// all ASCII, as a flat string's text is.
func segTextOf(u []uint16) *segText {
	b := make([]byte, len(u))
	for i, c := range u {
		if c >= 0x80 {
			return &segText{u: u}
		}
		b[i] = byte(c)
	}
	return &segText{ascii: string(b)}
}

// segBoundaries returns the boundaries forward iteration from 0 finds, 0
// and t.len() included, and the word-like flag of each segment. It fails
// the test if segNext does not advance or splits a surrogate pair.
func segBoundaries(t testing.TB, g segGranularity, st *segText) ([]int, []bool) {
	t.Helper()
	n := st.len()
	bounds := []int{0}
	var like []bool
	for i := 0; i < n; {
		e, wl := segNext(g, st, i)
		if e <= i || e > n {
			t.Fatalf("%s: segNext(%d) = %d of %d in %x", segNames[g], i, e, n, segUnits(st))
		}
		if e < n && st.at(e)&0xFC00 == 0xDC00 && st.at(e-1)&0xFC00 == 0xD800 {
			t.Fatalf("%s: boundary %d splits a surrogate pair in %x", segNames[g], e, segUnits(st))
		}
		bounds = append(bounds, e)
		like = append(like, wl)
		i = e
	}
	return bounds, like
}

// segCheckStarts checks segStart at every index of st against the
// boundaries of forward iteration.
func segCheckStarts(t testing.TB, g segGranularity, st *segText, bounds []int) {
	t.Helper()
	k := 0
	for i := range st.len() {
		for bounds[k+1] <= i {
			k++
		}
		if s := segStart(g, st, i); s != bounds[k] {
			t.Fatalf("%s: segStart(%d) = %d, want %d in %x", segNames[g], i, s, bounds[k], segUnits(st))
		}
	}
}

func segUnits(st *segText) []uint16 {
	u := make([]uint16, st.len())
	for i := range u {
		u[i] = st.at(i)
	}
	return u
}

// segBreakTest is one line of a UAX #29 break test: the code units and the
// boundaries (÷) between them.
type segBreakTest struct {
	units  []uint16
	bounds []int
	line   string
}

// loadSegBreakTest reads one of the break tests written by go generate
// (internal/gen/ucd -segtest).
func loadSegBreakTest(t *testing.T, file string) []segBreakTest {
	t.Helper()
	f, err := os.Open("testdata/" + file)
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	var tests []segBreakTest
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		bt := segBreakTest{line: line}
		for f := range strings.FieldsSeq(line) {
			switch f {
			case "÷":
				bt.bounds = append(bt.bounds, len(bt.units))
			case "×":
			default:
				c, err := strconv.ParseUint(f, 16, 32)
				require.NoError(t, err, line)
				bt.units = utf16.AppendRune(bt.units, rune(c))
			}
		}
		tests = append(tests, bt)
	}
	require.NoError(t, sc.Err())
	return tests
}

// TestSegConformance runs the UAX #29 break tests: every boundary and
// non-boundary of every line, through forward iteration and segStart at
// every index, for the ASCII and the UTF-16 forms of the text.
func TestSegConformance(t *testing.T) {
	for g, file := range [...]string{"grapheme_break_test.txt.gz", "word_break_test.txt.gz", "sentence_break_test.txt.gz"} {
		g := segGranularity(g)
		t.Run(segNames[g], func(t *testing.T) {
			tests := loadSegBreakTest(t, file)
			require.NotEmpty(t, tests)
			positions, failed := 0, 0
			for _, bt := range tests {
				forms := []*segText{{u: bt.units}}
				if st := segTextOf(bt.units); st.u == nil {
					forms = append(forms, st)
				}
				for _, st := range forms {
					got, _ := segBoundaries(t, g, st)
					if !slices.Equal(got, bt.bounds) {
						failed++
						t.Errorf("%s\n got %v\nwant %v", bt.line, got, bt.bounds)
						continue
					}
					segCheckStarts(t, g, st, got)
				}
				positions += len(bt.units) + 1
			}
			t.Logf("%d lines, %d positions, %d failed", len(tests), positions, failed)
		})
	}
}

// segJSONUnits decodes the JSON string raw to UTF-16 code units, keeping the
// lone surrogates its \u escapes can hold.
func segJSONUnits(raw []byte) ([]uint16, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, fmt.Errorf("not a JSON string: %s", raw)
	}
	s := raw[1 : len(raw)-1]
	var u []uint16
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			r, n := utf8.DecodeRune(s[i:])
			u = utf16.AppendRune(u, r)
			i += n
			continue
		}
		if i+1 == len(s) {
			return nil, fmt.Errorf("bad escape in %s", raw)
		}
		if s[i+1] == 'u' && i+6 <= len(s) {
			v, err := strconv.ParseUint(string(s[i+2:i+6]), 16, 16)
			if err != nil {
				return nil, err
			}
			u = append(u, uint16(v))
			i += 6
			continue
		}
		c, ok := map[byte]uint16{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[s[i+1]]
		if !ok {
			return nil, fmt.Errorf("bad escape in %s", raw)
		}
		u = append(u, c)
		i += 2
	}
	return u, nil
}

// segDictionary reports whether ICU segments words of c with a dictionary
// or keeps it out of its letters ($dictionary in word.txt: Han, Hiragana,
// Katakana, Hangul syllables and the Complex_Context scripts), where
// segment.go has the UAX #29 boundaries. The scripts are Go's.
func segDictionary(c rune) bool {
	return unicode.In(c, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao,
		unicode.Khmer, unicode.Myanmar, unicode.Tai_Tham, unicode.Tai_Le, unicode.New_Tai_Lue, unicode.Tai_Viet) ||
		c >= 0xAC00 && c <= 0xD7A3 || segWords().of(c)&31 == segWKatakana
}

// TestSegICU compares the segmentation with Node's (ICU's) on the corpus of
// testdata/seg_icu.js: grapheme and sentence boundaries everywhere, word
// boundaries except next to a segDictionary character, and isWordLike of
// every word segment both have.
func TestSegICU(t *testing.T) {
	f, err := os.Open("testdata/seg_icu.json.gz")
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	data, err := io.ReadAll(zr)
	require.NoError(t, err)
	var fixture struct {
		Node, ICU, Unicode string
		Cases              []struct {
			S       json.RawMessage
			G, W, T []int
			L       string
		}
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	t.Logf("Node %s, ICU %s, Unicode %s: %d strings", fixture.Node, fixture.ICU, fixture.Unicode, len(fixture.Cases))
	var positions, skipped, likes [3]int
	for _, c := range fixture.Cases {
		u, err := segJSONUnits(c.S)
		require.NoError(t, err)
		st := segTextOf(u)
		for g, want := range [][]int{c.G, c.W, c.T} {
			g := segGranularity(g)
			got, like := segBoundaries(t, g, st)
			got = got[1:]
			if g != segWord {
				positions[g] += len(u)
				if !slices.Equal(got, want) {
					t.Errorf("%s %s:\n got %v\nwant %v", segNames[g], c.S, got, want)
				}
				continue
			}
			for p := 1; p <= len(u); p++ {
				if slices.Contains(got, p) == slices.Contains(want, p) {
					positions[g]++
					continue
				}
				before := rune(-1)
				for k := p; k > 0; {
					var r rune
					r, k = st.before(0, k)
					if !segExFm(segWords().of(r) & 31) {
						before = r
						break
					}
				}
				after, _ := st.next(p)
				if !segDictionary(before) && !segDictionary(after) {
					t.Errorf("word %s: boundary at %d\n got %v\nwant %v", c.S, p, got, want)
				}
				skipped[g]++
			}
			// isWordLike of the segments both have.
			for k, e := range got {
				start := 0
				if k > 0 {
					start = got[k-1]
				}
				j := slices.Index(want, e)
				if j < 0 || (j == 0) != (start == 0) || j > 0 && want[j-1] != start {
					continue
				}
				likes[g]++
				if like[k] != (c.L[j] == '1') {
					t.Errorf("word %s: segment %d-%d isWordLike %v, ICU %c", c.S, start, e, like[k], c.L[j])
				}
			}
		}
	}
	t.Logf("positions equal: grapheme %d, word %d, sentence %d; word positions next to dictionary characters that differ: %d; isWordLike of %d segments",
		positions[0], positions[1], positions[2], skipped[1], likes[1])
}

// segAlphabet is the code points FuzzSegment builds texts from, one per
// byte: something of every class of the three tables and the code points
// of their context rules.
var segAlphabet = []rune{
	'a', 'Z', 'x', '0', '7', '.', ',', ':', ';', '\'', '"', '_', '-', '!', '?', ')', '(', ' ', '\t',
	'\r', '\n', 0x85, 0x2028, 0x2029, 0x0B,
	0x0300, 0x0308, 0x200D, 0x200C, 0x00AD, 0x2060, 0xFE0F, 0x1F3FB, // Extend, ZWJ, ZWNJ, Format, VS16, skin tone
	0x1F1E6, 0x1F1FA, 0x1F600, 0x2764, 0x1F468, 0x2139, 0x00A9, // RI, ExtPict
	0x1100, 0x1161, 0x11A8, 0xAC00, 0xAC01, // Hangul L V T LV LVT
	0x0915, 0x094D, 0x0937, 0x093C, 0x0940, 0x093F, 0x0600, 0x0E01, 0x0E31, 0x0E33, // Indic, Prepend, Thai
	0x05D0, 0x05D1, 0x05F3, // Hebrew
	0x30A2, 0x30FC, 0x3042, 0x4E00, 0x3005, 0x3002, 0xFF0E, // kana, ideographs, ideographic full stop
	0x3000, 0x1680, 0x2019, 0x2024, 0xFE52, 0x066C, 0x00B7, 0x0589, // spaces, quotes, Mid*
	0xD800, 0xDBFF, 0xDC00, 0xDFFF, 0x10000, 0x1D7CE, 0xE0001, 0xE0020, // surrogates, supplementary, tags
	'A', 'b', 0x00C0, 0x00E9, 0x01C5, 0x2163, 0x00BB, 0x201D, 0x0F3A, // Upper, Lower, Close
}

// FuzzSegment checks every granularity on arbitrary text: segNext advances
// and never splits a surrogate pair, segStart agrees with forward iteration
// at every index, and the ASCII form of a text segments as its UTF-16 form.
// An even first byte reads the rest as little-endian code units, an odd one
// as indexes into segAlphabet.
func FuzzSegment(f *testing.F) {
	for _, s := range []string{
		"", "a", "\x00a\x00", "\x01\x00\x01\x02\x03", "\x01\x19\x14\x19\x1a\x1f\x19\x14",
		"\x00\x00\xd8\x00\xdc\x00\xd8a\x00\x00\xdc",
		"\x01\x1c\x1c\x1c\x1d\x1d\x1d\x1e\x1e", "\x01\x24\x25\x26\x25\x26", "\x01\x2b\x2c\x26\x2c\x2d",
		"\x01\x00\x05\x11\x00\x0e\x03\x05", "\x01\x31\x09\x0a\x31", "\x01\x5d\x05\x11\x5e\x5f\x11\x5d",
		"\x01\x2e\x2f\x30\x2f\x30\x2e\x2f", "\x01\x1f\x1f\x1f\x1f\x1f\x1d\x1f",
		"\x01\x00\x05\x0f\x11\x5f\x5f\x11\x5e",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) == 0 || len(b) > 512 {
			return
		}
		var u []uint16
		if b[0]&1 == 0 {
			for i := 1; i+1 < len(b); i += 2 {
				u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
			}
		} else {
			for _, c := range b[1:] {
				u = utf16.AppendRune(u, segAlphabet[int(c)%len(segAlphabet)])
			}
		}
		if len(u) == 0 {
			return
		}
		segCheckSentenceSkip(t, &segText{u: u})
		for g := range segGranularity(3) {
			st := &segText{u: u}
			bounds, like := segBoundaries(t, g, st)
			segCheckStarts(t, g, st, bounds)
			if a := segTextOf(u); a.u == nil {
				ab, al := segBoundaries(t, g, a)
				if !slices.Equal(ab, bounds) || !slices.Equal(al, like) {
					t.Fatalf("%s: ASCII form %v %v, UTF-16 form %v %v", segNames[g], ab, al, bounds, like)
				}
				segCheckStarts(t, g, a, ab)
			}
		}
	})
}

// segSentenceNextRef is segSentenceNext without segSentenceSkip: every
// code point through the rules.
func segSentenceNextRef(t *segText, i int) int {
	s := segSentences()
	n := t.len()
	var term uint8
	closeSp, sp, upperLower := false, false, false
	x := segSOther
	for j := i; j < n; {
		c, k := t.next(j)
		y := s.of(c)
		if j > i && (y == segSExtend || y == segSFormat) {
			j = k
			continue
		}
		if term != 0 {
			switch {
			case y == segSSContinue || y == segSSTerm || y == segSATerm,
				y == segSClose && !sp,
				y == segSSp, segParaSep(y),
				term == segSATerm && !closeSp && y == segSNumeric,
				term == segSATerm && !closeSp && upperLower && y == segSUpper,
				term == segSATerm && segSB8(t, s, j):
			default:
				return j
			}
		}
		switch {
		case y == segSCR:
			if k < n && t.at(k) == '\n' {
				return k + 1
			}
			return k
		case segParaSep(y):
			return k
		case y == segSATerm || y == segSSTerm:
			term, closeSp, sp = y, false, false
			upperLower = x == segSUpper || x == segSLower
		case term != 0 && y == segSClose && !sp:
			closeSp = true
		case term != 0 && y == segSSp:
			closeSp, sp = true, true
		default:
			term = 0
		}
		x, j = y, k
	}
	return n
}

// segCheckSentenceSkip checks segSentenceNext against segSentenceNextRef
// from every index of t that starts a code point.
func segCheckSentenceSkip(t *testing.T, st *segText) {
	t.Helper()
	for i := 0; i < st.len(); i++ {
		if st.u != nil && i > 0 && st.u[i]&0xFC00 == 0xDC00 && st.u[i-1]&0xFC00 == 0xD800 {
			continue
		}
		if got, want := segSentenceNext(st, i), segSentenceNextRef(st, i); got != want {
			t.Fatalf("sentence from %d of %U: %d, every code point through the rules %d", i, st.u, got, want)
		}
	}
}

// TestSegSentenceSkip checks that skipping to the next terminator or
// paragraph separator finds the sentences that applying the rules to every
// code point finds, on random texts that favour the classes x and SB5 see.
func TestSegSentenceSkip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	favour := []rune{'.', '!', ' ', 'a', 'A', '1', 0x0300, 0x00AD, ')', '\r', '\n', 0x2029}
	for range 20000 {
		var u []uint16
		for range 1 + rng.IntN(24) {
			c := segAlphabet[rng.IntN(len(segAlphabet))]
			if rng.IntN(2) == 0 {
				c = favour[rng.IntN(len(favour))]
			}
			u = utf16.AppendRune(u, c)
		}
		segCheckSentenceSkip(t, &segText{u: u})
		if a := segTextOf(u); a.u == nil {
			segCheckSentenceSkip(t, a)
		}
	}
}

// TestSegSentenceASCIIStops checks segSentenceASCIIStops against the
// sentence table.
func TestSegSentenceASCIIStops(t *testing.T) {
	s := segSentences()
	for c := range rune(0x80) {
		stop := segSentenceStops>>s.of(c)&1 != 0
		require.Equal(t, stop, c < 64 && segSentenceASCIIStops>>c&1 != 0, "%U", c)
	}
}

// TestSegAllocs checks that segNext and segStart do not allocate.
func TestSegAllocs(t *testing.T) {
	u := utf16.Encode([]rune("Hello, wörld! 你好。👨‍👩‍👧 🇯🇵 क्षि"))
	texts := []*segText{{u: u}, {ascii: "Mr. Smith said: \"It's 3.14.\" Then left.\r\n"}}
	for g := range segGranularity(3) {
		for _, st := range texts {
			segNext(g, st, 0) // decode the tables
			allocs := testing.AllocsPerRun(10, func() {
				for i := 0; i < st.len(); {
					i, _ = segNext(g, st, i)
				}
				for i := range st.len() {
					segStart(g, st, i)
				}
			})
			require.Zero(t, allocs, segNames[g])
		}
	}
}

func Example_segNext() {
	st := &segText{u: utf16.Encode([]rune("Don't panic! 👍🏽 नमस्ते"))}
	for g := range segGranularity(3) {
		var segs []string
		for i := 0; i < st.len(); {
			e, like := segNext(g, st, i)
			s := string(utf16.Decode(st.u[i:e]))
			if like {
				s += "*"
			}
			segs = append(segs, strconv.Quote(s))
			i = e
		}
		fmt.Println(segNames[g], strings.Join(segs, " "))
	}
	// Output:
	// grapheme "D" "o" "n" "'" "t" " " "p" "a" "n" "i" "c" "!" " " "👍🏽" " " "न" "म" "स्ते"
	// word "Don't*" " " "panic*" "!" " " "👍🏽" " " "नमस्ते*"
	// sentence "Don't panic! " "👍🏽 नमस्ते"
}

// segBenchText returns testdata/seg_sample.txt (mixed Latin, CJK, Hangul,
// Indic, Arabic, Hebrew and emoji) repeated to at least 64 KiB, and its
// length in UTF-8.
func segBenchText(b *testing.B) (*segText, int) {
	sample, err := os.ReadFile("testdata/seg_sample.txt")
	require.NoError(b, err)
	s := strings.Repeat(string(sample), (64<<10+len(sample)-1)/len(sample))
	return &segText{u: utf16.Encode([]rune(s))}, len(s)
}

func benchmarkSeg(b *testing.B, g segGranularity) {
	st, size := segBenchText(b)
	segs := 0
	for i := 0; i < st.len(); segs++ {
		i, _ = segNext(g, st, i)
	}
	b.SetBytes(int64(size))
	for b.Loop() {
		for i := 0; i < st.len(); {
			i, _ = segNext(g, st, i)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(segs), "ns/seg")
}

func BenchmarkSegGrapheme(b *testing.B) { benchmarkSeg(b, segGrapheme) }
func BenchmarkSegWord(b *testing.B)     { benchmarkSeg(b, segWord) }
func BenchmarkSegSentence(b *testing.B) { benchmarkSeg(b, segSentence) }

// BenchmarkSegDecode measures the one-time decoding of each table.
func BenchmarkSegDecode(b *testing.B) {
	for g, decode := range []func(){
		func() { new(segGraphemeTables).decode() },
		func() { new(segWordTables).decode() },
		func() { new(segTrie).decode(segSentenceTrie) },
	} {
		b.Run(segNames[g], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				decode()
			}
		})
	}
}

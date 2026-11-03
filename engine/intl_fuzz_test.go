package engine

// Fuzz targets for Intl.NumberFormat, PluralRules and DateTimeFormat at the
// JavaScript level, and for the decimal string parser against a big.Rat
// reference. Each target holds one realm per fuzz worker; a thrown
// RangeError or TypeError is an accepted outcome, a Go panic or a broken
// invariant (format equal to its parts, the same formatter from
// resolvedOptions) is a failure.

import (
	"encoding/binary"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

type intlFuzzRealm struct {
	sync.Mutex
	r  *Realm
	fn Value
}

func (st *intlFuzzRealm) call(t *testing.T, src string, args ...Value) string {
	st.Lock()
	defer st.Unlock()
	if st.r == nil {
		m, err := syntax.ParseModule("fuzz.js", src, syntax.Options{})
		if err != nil {
			t.Fatal(err)
		}
		code, err := compiler.CompileModule(m)
		if err != nil {
			t.Fatal(err)
		}
		st.r = NewRealmWith(RealmOptions{})
		env, err := st.r.EvaluateModule(code)
		if err != nil {
			t.Fatal(err)
		}
		st.fn, _ = env.GetBindingValue("f")
	}
	res, err := st.r.Call(st.fn, Undefined(), args)
	if err != nil {
		var exc *Exception
		if asException(err, &exc) {
			t.Fatalf("%s", errorDisplayString(exc.Value))
		}
		t.Fatal(err)
	}
	s, _ := st.r.ToString(res)
	return s.GoString()
}

// intlFuzzPick reads choices from fuzz bytes.
type intlFuzzPick struct{ b []byte }

func (p *intlFuzzPick) n(k int) int {
	if len(p.b) == 0 {
		return 0
	}
	c := p.b[0]
	p.b = p.b[1:]
	return int(c) % k
}

// opt returns -1 (the option is absent) for three bytes in four and once
// the bytes run out, else 0..k, k standing for undefined.
func (p *intlFuzzPick) opt(k int) int {
	if len(p.b) == 0 {
		return -1
	}
	c := p.b[0]
	p.b = p.b[1:]
	if c%4 != 0 {
		return -1
	}
	return int(c/4) % (k + 1)
}

const intlFuzzNFSrc = `
const pick = (list, i) => list[i % list.length];
export function f(loc, optsJSON, kind, a, b) {
  const opts = JSON.parse(optsJSON);
  for (const k in opts) if (opts[k] === "<u>") opts[k] = undefined;
  let nf;
  try { nf = new Intl.NumberFormat(loc, opts) } catch (e) { if (e instanceof RangeError || e instanceof TypeError) return "err"; throw e }
  const conv = (v) => kind === 1 ? v : kind === 2 ? BigInt(v) : Number(v);
  let x, y;
  try { x = conv(a); y = conv(b) } catch (e) { x = Number(a); y = Number(b) }
  const s = nf.format(x), p = nf.formatToParts(x);
  const j = p.map(q => q.value).join("");
  if (s !== j) throw new Error("parts " + JSON.stringify(s) + " vs " + JSON.stringify(j) + " " + optsJSON + " " + a);
  for (const q of p) if (typeof q.type !== "string" || typeof q.value !== "string" || q.value === "") throw new Error("part " + JSON.stringify(q));
  const ro = nf.resolvedOptions();
  let nf2;
  try { nf2 = new Intl.NumberFormat(ro.locale, ro) } catch (e) { throw new Error("resolvedOptions not reusable: " + e + " " + JSON.stringify(ro)) }
  const s2 = nf2.format(x);
  if (s2 !== s) throw new Error("resolvedOptions " + JSON.stringify(s) + " vs " + JSON.stringify(s2) + " " + JSON.stringify(ro) + " " + a);
  let r = s;
  try {
    const rs = nf.formatRange(x, y), rp = nf.formatRangeToParts(x, y);
    if (rs !== rp.map(q => q.value).join("")) throw new Error("range parts " + rs + " " + optsJSON);
    for (const q of rp) if (!["startRange", "endRange", "shared"].includes(q.source)) throw new Error("source " + JSON.stringify(q));
    r += "|" + rs;
  } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  let pr;
  try { pr = new Intl.PluralRules(loc, opts) } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  if (pr) {
    const c = pr.select(x);
    if (!pr.resolvedOptions().pluralCategories.includes(c)) throw new Error("category " + c);
    try { pr.selectRange(x, y) } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  }
  return r;
}`

var (
	intlFuzzLocales = []string{"en", "en-US", "en-GB", "en-IN", "de", "de-CH", "de-AT", "fr", "fr-CA", "fr-CH", "ja", "ja-JP", "zh", "zh-CN", "zh-Hans-SG", "zh-TW", "zh-HK", "ko", "es", "es-MX", "es-419", "pt", "pt-PT", "ru", "it", "ar", "ar-EG", "hi", "th", "und", "xx", "en-u-nu-arab", "en-u-nu-thai", "zh-u-nu-hanidec", "ja-u-nu-fullwide", "de-u-nu-deva", "en-u-hc-h11", "ja-u-hc-h24", "en-u-ca-gregory", "en-u-ca-japanese", "fa", "he", "sr-Latn"}
	intlFuzzNFOpts  = [][2]any{
		{"style", []any{"decimal", "percent", "currency", "unit"}},
		{"currency", []any{"USD", "JPY", "KWD", "CLF", "EUR", "CNY", "XXX", "BHD", "KRW", "TWD", "abc", "XAU", "BYR", "ZWL"}},
		{"currencyDisplay", []any{"symbol", "narrowSymbol", "code", "name"}},
		{"currencySign", []any{"standard", "accounting"}},
		{"unit", []any{"kilometer-per-hour", "percent", "byte", "liter-per-kilometer", "meter", "celsius", "mile-per-gallon", "gigabyte", "second", "day", "fluid-ounce", "kilobit-per-second", "degree", "acre"}},
		{"unitDisplay", []any{"short", "long", "narrow"}},
		{"notation", []any{"standard", "scientific", "engineering", "compact"}},
		{"compactDisplay", []any{"short", "long"}},
		{"signDisplay", []any{"auto", "never", "always", "exceptZero", "negative"}},
		{"useGrouping", []any{true, false, "min2", "always", "auto"}},
		{"minimumIntegerDigits", []any{1, 2, 5, 21}},
		{"minimumFractionDigits", []any{0, 1, 2, 3, 20, 100}},
		{"maximumFractionDigits", []any{0, 1, 2, 3, 5, 20, 100}},
		{"minimumSignificantDigits", []any{1, 2, 5, 21}},
		{"maximumSignificantDigits", []any{1, 2, 3, 5, 21}},
		{"roundingIncrement", []any{1, 2, 5, 10, 20, 25, 50, 100, 250, 500, 1000, 2500, 5000}},
		{"roundingMode", []any{"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven"}},
		{"roundingPriority", []any{"auto", "morePrecision", "lessPrecision"}},
		{"trailingZeroDisplay", []any{"auto", "stripIfInteger"}},
		{"numberingSystem", []any{"latn", "arab", "thai", "hanidec", "deva", "fullwide", "arabext", "beng"}},
	}
	intlFuzzNumbers = []string{"0", "-0", "1", "-1", "0.5", "1.5", "2.5", "-2.5", "999.5", "999999.5", "1e21", "-1e21", "5e-324", "1.7976931348623157e308", "-1.7976931348623157e308", "Infinity", "-Infinity", "NaN", "123456789.123456789", "0.000001234", "1e-7", "9.995", "99999", "100000", "1000000", "1e15", "12345678901234567890", "0.1", "1e100", "1e-100"}
)

// intlFuzzOptsJSON builds an options object from choices: each option is
// absent, undefined or one of its values.
func intlFuzzOptsJSON(p *intlFuzzPick, opts [][2]any) string {
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	for _, o := range opts {
		vals := o[1].([]any)
		k := p.opt(len(vals))
		if k < 0 {
			continue // absent most of the time
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(`"` + o[0].(string) + `":`)
		if k == len(vals) {
			sb.WriteString(`"<u>"`)
			continue
		}
		switch v := vals[k].(type) {
		case string:
			sb.WriteString(`"` + v + `"`)
		case bool:
			if v {
				sb.WriteString("true")
			} else {
				sb.WriteString("false")
			}
		case int:
			sb.WriteString(intlFuzzItoa(v))
		}
	}
	sb.WriteByte('}')
	return sb.String()
}

func intlFuzzItoa(v int) string { return big.NewInt(int64(v)).String() }

// intlFuzzValue makes a numeric value's source text from choices and raw
// bytes: a listed number, a float64's bits, or a digit string.
func intlFuzzValue(p *intlFuzzPick, raw []byte) string {
	switch p.n(4) {
	case 0:
		return intlFuzzNumbers[p.n(len(intlFuzzNumbers))]
	case 1:
		var b [8]byte
		copy(b[:], raw)
		f := math.Float64frombits(binary.LittleEndian.Uint64(b[:]))
		return intlFuzzFloat(f)
	default:
		var sb strings.Builder
		if p.n(3) == 0 {
			sb.WriteByte('-')
		}
		for i, c := range raw {
			if i > 40 {
				break
			}
			sb.WriteByte('0' + c%10)
			if c%23 == 0 && !strings.Contains(sb.String(), ".") {
				sb.WriteByte('.')
			}
		}
		if sb.Len() == 0 || sb.String() == "-" {
			sb.WriteByte('7')
		}
		if p.n(3) == 0 {
			sb.WriteString("e" + intlFuzzItoa(p.n(80)-40))
		}
		return sb.String()
	}
}

func intlFuzzFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return big.NewFloat(f).Text('g', -1)
}

var intlFuzzNFRealm intlFuzzRealm

func FuzzIntlNumberFormatJS(f *testing.F) {
	f.Add([]byte{0, 0, 3, 1, 2, 3, 4, 5, 6, 7, 8, 9}, []byte("12345"), []byte("67890"))
	f.Add([]byte{5, 2, 1, 0, 4, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 3, 2, 1}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xef, 0x7f}, []byte("1"))
	f.Add([]byte{12, 7, 7, 7, 7, 7, 7, 3, 2, 7, 7, 7, 7, 7, 7, 7, 7, 0, 7, 7, 7}, []byte("99999999999999999999"), []byte("0"))
	f.Add([]byte{17, 1, 0, 6, 6, 6, 6, 6, 6, 3, 6, 6, 6, 6, 6, 6, 6, 4, 6, 6, 6, 1}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("5"))
	f.Fuzz(func(t *testing.T, choices, raw1, raw2 []byte) {
		if len(choices) > 64 || len(raw1) > 64 || len(raw2) > 64 {
			return
		}
		p := &intlFuzzPick{b: choices}
		loc := intlFuzzLocales[p.n(len(intlFuzzLocales))]
		kind := p.n(3)
		opts := intlFuzzOptsJSON(p, intlFuzzNFOpts)
		a, b := intlFuzzValue(p, raw1), intlFuzzValue(p, raw2)
		intlFuzzNFRealm.call(t, intlFuzzNFSrc, StringValue(FromGoString(loc)), StringValue(FromGoString(opts)), IntValue(kind), StringValue(FromGoString(a)), StringValue(FromGoString(b)))
	})
}

const intlFuzzDTFSrc = `
export function f(loc, optsJSON, a, b) {
  const opts = JSON.parse(optsJSON);
  for (const k in opts) if (opts[k] === "<u>") opts[k] = undefined;
  let df;
  try { df = new Intl.DateTimeFormat(loc, opts) } catch (e) { if (e instanceof RangeError || e instanceof TypeError) return "err"; throw e }
  df.resolvedOptions();
  let r = "";
  let s, p;
  try { s = df.format(a); p = df.formatToParts(a) } catch (e) { if (e instanceof RangeError && (a !== a || Math.intlFuzzAbs(a) > 8.64e15)) return "range"; throw e }
  const j = p.map(q => q.value).join("");
  if (s.replace(/\u202f/g, " ") !== j.replace(/\u202f/g, " ")) throw new Error("parts " + JSON.stringify(s) + " vs " + JSON.stringify(j) + " " + optsJSON + " " + a);
  for (const q of p) if (typeof q.type !== "string" || q.value === "") throw new Error("part " + JSON.stringify(q));
  // No resolvedOptions round trip: V8 (and moejs, which follows it) reads
  // the fields from the pattern, appended items such as "(Monat: O)"
  // included, so a formatter made from them can differ.
  r = s;
  try {
    const rs = df.formatRange(a, b), rp = df.formatRangeToParts(a, b);
    if (rs.replace(/\u202f/g, " ") !== rp.map(q => q.value).join("").replace(/\u202f/g, " ")) throw new Error("range parts " + JSON.stringify(rs) + " vs " + JSON.stringify(rp) + " " + optsJSON + " " + a + " " + b);
    for (const q of rp) if (!["startRange", "endRange", "shared"].includes(q.source)) throw new Error("source " + JSON.stringify(q));
    r += "|" + rs;
  } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  return r;
}`

var (
	intlFuzzZones   = []any{"UTC", "America/New_York", "Asia/Tokyo", "Europe/London", "Australia/Lord_Howe", "Asia/Kolkata", "Asia/Calcutta", "Pacific/Chatham", "+05:30", "-14:00", "+14", "Etc/GMT+12", "Etc/GMT-14", "America/St_Johns", "Africa/Casablanca", "Antarctica/Troll", "Pacific/Apia", "America/Sao_Paulo", "Europe/Dublin", "Pacific/Kiritimati", "America/Los_Angeles", "Europe/Berlin", "Asia/Shanghai", "Asia/Taipei", "Asia/Seoul", "Europe/Moscow", "America/Caracas", "Asia/Kathmandu", "Etc/Unknown", "Factory", "EST5EDT", "US/Pacific", "Europe/Kyiv", "America/Argentina/Buenos_Aires"}
	intlFuzzDTFOpts = [][2]any{
		{"timeZone", intlFuzzZones},
		{"hour12", []any{true, false}},
		{"hourCycle", []any{"h11", "h12", "h23", "h24"}},
		{"weekday", []any{"narrow", "short", "long"}},
		{"era", []any{"narrow", "short", "long"}},
		{"year", []any{"numeric", "2-digit"}},
		{"month", []any{"numeric", "2-digit", "narrow", "short", "long"}},
		{"day", []any{"numeric", "2-digit"}},
		{"dayPeriod", []any{"narrow", "short", "long"}},
		{"hour", []any{"numeric", "2-digit"}},
		{"minute", []any{"numeric", "2-digit"}},
		{"second", []any{"numeric", "2-digit"}},
		{"fractionalSecondDigits", []any{1, 2, 3}},
		{"timeZoneName", []any{"short", "long", "shortOffset", "longOffset", "shortGeneric", "longGeneric"}},
		{"formatMatcher", []any{"basic", "best fit"}},
		{"dateStyle", []any{"full", "long", "medium", "short"}},
		{"timeStyle", []any{"full", "long", "medium", "short"}},
		{"numberingSystem", []any{"latn", "arab", "hanidec", "thai", "fullwide"}},
		{"calendar", []any{"gregory", "iso8601", "japanese"}},
	}
	// intlFuzzTimes are edges: the TimeClip range, the epoch, DST changes
	// (America/New_York 2024-03-10 07:00Z and 2024-11-03 06:00Z,
	// Lord_Howe's half hour, Dublin's negative DST), year 1, pre-1970.
	intlFuzzTimes = []float64{0, -1, 8.64e15, -8.64e15, 8.64e15 + 1, math.NaN(), 1710054000000, 1710053999999, 1730613600000, 1730613599999, 1730610000000, -62135596800000, -62135596800001, -2208988800000, -1e12, 1e12, 1696089600000, 1711814400000, 1743901200000, -377705116800000, 253402300799999, 253402300800000, 1.5e15, -6e13}
)

var intlFuzzDTFRealm intlFuzzRealm

func FuzzIntlDateTimeFormatJS(f *testing.F) {
	f.Add([]byte{0, 1, 0, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6}, uint64(0), uint64(3))
	f.Add([]byte{2, 3, 6, 6, 6, 0, 2, 1, 4, 0, 6, 0, 0, 0, 2, 3, 6, 6, 6, 6, 6}, uint64(1710054000000), uint64(1730613600000))
	f.Add([]byte{14, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 0, 0, 6, 6, 6}, uint64(5), uint64(7))
	f.Fuzz(func(t *testing.T, choices []byte, x, y uint64) {
		if len(choices) > 64 {
			return
		}
		p := &intlFuzzPick{b: choices}
		loc := intlFuzzLocales[p.n(len(intlFuzzLocales))]
		opts := intlFuzzOptsJSON(p, intlFuzzDTFOpts)
		val := func(u uint64) float64 {
			if u&1 == 0 {
				return intlFuzzTimes[(u>>1)%uint64(len(intlFuzzTimes))]
			}
			return float64(int64(u>>1)%(17280000000000001)) - 8.64e15
		}
		intlFuzzDTFRealm.call(t, intlFuzzDTFSrc, StringValue(FromGoString(loc)), StringValue(FromGoString(opts)), NumberValue(val(x)), NumberValue(val(y)))
	})
}

// FuzzIntlDecimalString checks setString against a big.Rat reference of
// ToIntlMathematicalValue with RoundMVResult's limits.
func FuzzIntlDecimalString(f *testing.F) {
	for _, s := range []string{"1", "-0", " 12.5e3 ", "0x1F", "0b101", "0o17", ".5", "5.", "1e-400", "1e400", "-Infinity", "+Infinity", "1e", "e5", "--1", "0.000e1000000", "1_0", "1.7976931348623157e308", "1.797693134862315807937289714053e308", "2.4703282292062327e-324", "2.4703282292062328e-324", " 1\ufeff", "0x", "0X10", "-0x10", "1e+", "00012.3400", "1.e5"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 400 {
			return
		}
		var d intlDecimal
		js := FromGoString(s)
		d.setString(js)
		kind, neg, val := intlRefMV(s)
		if d.kind != kind {
			t.Fatalf("%q: kind %d, want %d", s, d.kind, kind)
		}
		if kind == intlNaN {
			return
		}
		if kind == intlInfinity {
			if d.neg != neg {
				t.Fatalf("%q: infinity sign %v, want %v", s, d.neg, neg)
			}
			return
		}
		if d.neg != neg {
			t.Fatalf("%q: sign %v, want %v", s, d.neg, neg)
		}
		got := new(big.Rat)
		if len(d.digits) > 0 {
			m, _ := new(big.Int).SetString(string(d.digits), 10)
			e := d.exp - len(d.digits)
			p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(intlFuzzAbs(e))), nil)
			if e >= 0 {
				got.SetInt(m.Mul(m, p))
			} else {
				got.SetFrac(m, p)
			}
		}
		if got.Cmp(val) != 0 {
			t.Fatalf("%q: %s (digits %q exp %d), want %s", s, got.FloatString(30), d.digits, d.exp, val.FloatString(30))
		}
	})
}

func intlFuzzAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// intlRefMV is the reference: StringToNumber's grammar on JS white space,
// the exact value as a big.Rat (absolute), and RoundMVResult's limits.
func intlRefMV(s string) (kind uint8, neg bool, val *big.Rat) {
	u := []rune(s)
	isWS := func(c rune) bool {
		return c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r' || c == ' ' || c == 0xa0 || c == 0x1680 || c >= 0x2000 && c <= 0x200a || c == 0x2028 || c == 0x2029 || c == 0x202f || c == 0x205f || c == 0x3000 || c == 0xfeff
	}
	for len(u) > 0 && isWS(u[0]) {
		u = u[1:]
	}
	for len(u) > 0 && isWS(u[len(u)-1]) {
		u = u[:len(u)-1]
	}
	t := string(u)
	val = new(big.Rat)
	switch t {
	case "":
		return intlFinite, false, val
	case "Infinity", "+Infinity":
		return intlInfinity, false, nil
	case "-Infinity":
		return intlInfinity, true, nil
	}
	for _, c := range t {
		if c >= 0x80 {
			return intlNaN, false, nil
		}
	}
	if len(t) > 2 && t[0] == '0' && strings.ContainsRune("xXoObB", rune(t[1])) {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[t[1]|0x20]
		var v big.Int
		if _, ok := v.SetString(t[2:], base); !ok || strings.ContainsAny(t[2:], "+-_") {
			return intlNaN, false, nil
		}
		val.SetInt(&v)
		return intlRefClamp(false, val)
	}
	i := 0
	if t[0] == '+' || t[0] == '-' {
		neg = t[0] == '-'
		i++
	}
	mant, frac, any := "", "", false
	for ; i < len(t) && t[i] >= '0' && t[i] <= '9'; i++ {
		mant += t[i : i+1]
		any = true
	}
	if i < len(t) && t[i] == '.' {
		for i++; i < len(t) && t[i] >= '0' && t[i] <= '9'; i++ {
			frac += t[i : i+1]
			any = true
		}
	}
	if !any {
		return intlNaN, false, nil
	}
	e := int64(0)
	if i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		i++
		eneg := false
		if i < len(t) && (t[i] == '+' || t[i] == '-') {
			eneg = t[i] == '-'
			i++
		}
		if i == len(t) {
			return intlNaN, false, nil
		}
		for ; i < len(t) && t[i] >= '0' && t[i] <= '9'; i++ {
			if e < 1e9 {
				e = e*10 + int64(t[i]-'0')
			}
		}
		if eneg {
			e = -e
		}
	}
	if i != len(t) {
		return intlNaN, false, nil
	}
	digits := strings.TrimLeft(mant+frac, "0")
	if digits == "" {
		return intlFinite, neg, val
	}
	// The exponent of the value's leading digit decides the far cases.
	lead := int64(len(mant)) - int64(len(mant+frac)-len(digits)) + e
	if lead > 400 {
		return intlInfinity, neg, nil
	}
	if lead < -400 {
		return intlFinite, neg, val
	}
	m, _ := new(big.Int).SetString(digits, 10)
	sc := lead - int64(len(digits))
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(intlFuzzAbs(int(sc)))), nil)
	if sc >= 0 {
		val.SetInt(m.Mul(m, p))
	} else {
		val.SetFrac(m, p)
	}
	return intlRefClamp(neg, val)
}

func intlRefClamp(neg bool, v *big.Rat) (uint8, bool, *big.Rat) {
	hi := new(big.Int).Lsh(big.NewInt(1), 1024)
	hi.Sub(hi, new(big.Int).Lsh(big.NewInt(1), 970))
	if v.Cmp(new(big.Rat).SetInt(hi)) >= 0 {
		return intlInfinity, neg, nil
	}
	lo := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), 1075))
	if v.Sign() != 0 && v.Cmp(lo) <= 0 {
		return intlFinite, neg, new(big.Rat)
	}
	return intlFinite, neg, v
}

// TestIntlFuzzDistribution logs how often random fuzz inputs construct a
// formatter (MOEJS_INTL_FUZZ_DIST=1), to check the generators reach the
// format paths and not only the option errors.
func TestIntlFuzzDistribution(t *testing.T) {
	if os.Getenv("MOEJS_INTL_FUZZ_DIST") != "1" {
		t.Skip("MOEJS_INTL_FUZZ_DIST=1")
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	bytesOf := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rnd.Uint32())
		}
		return b
	}
	nf, dtf := map[string]int{}, map[string]int{}
	for range 3000 {
		p := &intlFuzzPick{b: bytesOf(40)}
		loc := intlFuzzLocales[p.n(len(intlFuzzLocales))]
		kind := p.n(3)
		opts := intlFuzzOptsJSON(p, intlFuzzNFOpts)
		a, b := intlFuzzValue(p, bytesOf(12)), intlFuzzValue(p, bytesOf(12))
		out := intlFuzzNFRealm.call(t, intlFuzzNFSrc, StringValue(FromGoString(loc)), StringValue(FromGoString(opts)), IntValue(kind), StringValue(FromGoString(a)), StringValue(FromGoString(b)))
		if out != "err" {
			out = "ok"
		}
		nf[out]++
		p = &intlFuzzPick{b: bytesOf(40)}
		loc = intlFuzzLocales[p.n(len(intlFuzzLocales))]
		opts = intlFuzzOptsJSON(p, intlFuzzDTFOpts)
		out = intlFuzzDTFRealm.call(t, intlFuzzDTFSrc, StringValue(FromGoString(loc)), StringValue(FromGoString(opts)), NumberValue(float64(rnd.Int64N(17280000000000001))-8.64e15), NumberValue(intlFuzzTimes[rnd.IntN(len(intlFuzzTimes))]))
		if out != "err" && out != "range" {
			out = "ok"
		}
		dtf[out]++
	}
	t.Logf("NumberFormat %v, DateTimeFormat %v", nf, dtf)
}

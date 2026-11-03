package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntlDateTimeFormatShape(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"constructor", `return [typeof Intl.DateTimeFormat, Intl.DateTimeFormat.length, Intl.DateTimeFormat.name, Intl.DateTimeFormat.supportedLocalesOf.length, Object.getOwnPropertyNames(Intl.DateTimeFormat.prototype).sort().join(" ")].join()`,
			"function,0,DateTimeFormat,1,constructor format formatRange formatRangeToParts formatToParts resolvedOptions"},
		{"tag", `return Object.prototype.toString.call(new Intl.DateTimeFormat())`, "[object Intl.DateTimeFormat]"},
		{"call", `const f = Intl.DateTimeFormat("en", {timeZone: "UTC"}); return f instanceof Intl.DateTimeFormat && f.format(0)`, "1/1/1970"},
		{"format getter", `const f = new Intl.DateTimeFormat("en", {timeZone: "UTC"}); const g = f.format; const d = Object.getOwnPropertyDescriptor(Intl.DateTimeFormat.prototype, "format"); return [g === f.format, g.name, g.length, g(86400000), d.get.name, typeof d.set].join()`, "true,,1,1/2/1970,get format,undefined"},
		{"receiver", `return Intl.DateTimeFormat.prototype.formatToParts.call({}, 0)`, "!TypeError: Method Intl.DateTimeFormat.prototype.formatToParts called on incompatible receiver"},
		{"subclass", `class D extends Intl.DateTimeFormat {} const d = new D("de", {timeZone: "UTC"}); return [d instanceof D, d.format(0)].join()`, "true,1.1.1970"},
		{"supportedLocalesOf", `return Intl.DateTimeFormat.supportedLocalesOf(["zxx", "de-AT", "ja-JP", "zh-TW", "hi"]).join()`, "de-AT,ja-JP,zh-TW"},
	})
}

func TestIntlDateTimeFormat(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"issue", `return JSON.stringify(new Intl.DateTimeFormat("en-US", {timeZone: "Asia/Tokyo", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", hourCycle: "h23"}).formatToParts(new Date("2026-10-22T00:00:00Z")))`,
			`[{"type":"month","value":"10"},{"type":"literal","value":"/"},{"type":"day","value":"22"},{"type":"literal","value":"/"},{"type":"year","value":"2026"},{"type":"literal","value":", "},{"type":"hour","value":"09"}]`},
		{"default", `return new Intl.DateTimeFormat("en", {timeZone: "UTC"}).format(Date.UTC(2026, 9, 22, 13))`, "10/22/2026"},
		{"styles", `const d = Date.UTC(2026, 9, 22, 13, 5, 7); return ["en", "de", "ja", "zh-Hans", "ko"].map(l => new Intl.DateTimeFormat(l, {dateStyle: "medium", timeStyle: "short", timeZone: "UTC"}).format(d)).join("|")`,
			"Oct 22, 2026, 1:05 PM|22.10.2026, 13:05|2026/10/22 13:05|2026年10月22日 13:05|2026. 10. 22. 오후 1:05"},
		{"nnbsp in parts", `const p = new Intl.DateTimeFormat("en", {hour: "numeric", timeZone: "UTC"}).formatToParts(0); return p.map(x => x.value.charCodeAt(0).toString(16)).join()`, "31,202f,41"},
		{"at", `return new Intl.DateTimeFormat("en", {year: "numeric", month: "long", day: "numeric", hour: "numeric", minute: "2-digit", timeZone: "UTC"}).format(0)`, "January 1, 1970 at 12:00 AM"},
		{"hour12", `return [true, false].map(h => new Intl.DateTimeFormat("ja", {hour: "numeric", hour12: h, timeZone: "UTC"}).resolvedOptions().hourCycle).join()`, "h11,h23"},
		{"hourCycle", `return ["h11", "h12", "h23", "h24"].map(hc => new Intl.DateTimeFormat("en", {hour: "numeric", minute: "2-digit", hourCycle: hc, timeZone: "UTC"}).format(0)).join("|")`, "0:00 AM|12:00 AM|00:00|24:00"},
		{"dayPeriod", `return [9, 12, 15, 21].map(h => new Intl.DateTimeFormat("en", {hour: "numeric", dayPeriod: "long", timeZone: "UTC"}).format(Date.UTC(2026, 0, 1, h))).join("|")`, "9 in the morning|12 noon|3 in the afternoon|9 at night"},
		{"fractional", `return new Intl.DateTimeFormat("en", {minute: "2-digit", second: "2-digit", fractionalSecondDigits: 2, timeZone: "UTC"}).format(61234)`, "01:01.23"},
		{"era", `return new Intl.DateTimeFormat("en", {year: "numeric", era: "short", timeZone: "UTC"}).format(Date.UTC(-500, 0, 1))`, "501 BC"},
		{"numbering", `return new Intl.DateTimeFormat("en-u-nu-arab", {hour: "numeric", minute: "numeric", second: "numeric", fractionalSecondDigits: 3, timeZone: "UTC"}).format(3723456)`, "١:٠٢:٠٣٫٤٥٦ AM"},
		{"range", `const f = new Intl.DateTimeFormat("en", {dateStyle: "medium", timeZone: "UTC"}); return [f.formatRange(0, 86400000 * 3), f.formatRange(0, 3600000), JSON.stringify(f.formatRangeToParts(0, 86400000 * 40).map(p => p.source[0]).join(""))].join("|")`,
			"Jan 1\u2009–\u20094, 1970|Jan 1, 1970|\"sssseeess\""},
		{"resolvedOptions", `return JSON.stringify(new Intl.DateTimeFormat("de-u-nu-thai-ca-gregory", {hour: "2-digit", minute: "numeric", timeZone: "UTC"}).resolvedOptions())`,
			`{"locale":"de-u-ca-gregory-nu-thai","calendar":"gregory","numberingSystem":"thai","timeZone":"UTC","hourCycle":"h23","hour12":false,"hour":"2-digit","minute":"2-digit"}`},
		{"regional zh", `const f = l => new Intl.DateTimeFormat(l, {weekday: "short", hour: "numeric", timeZone: "UTC"}); return ["zh-TW", "zh-HK", "zh-MO", "zh-Hant-HK", "zh-SG", "zh-CN"].map(l => f(l).resolvedOptions().locale + " " + f(l).format(0)).join("|")`,
			"zh-TW 週四上午12時|zh-HK 週四上午12時|zh-MO 週四上午12時|zh-Hant-HK 週四上午12時|zh-SG 周四上午12时|zh-CN 周四 0时"},
		{"regional hours", `return ["en-GB", "en-IN", "pt-PT", "es-419"].map(l => new Intl.DateTimeFormat(l, {hour: "numeric", timeZone: "UTC"}).resolvedOptions().hourCycle).join()`, "h23,h12,h23,h12"},
		{"other calendar", `const o = new Intl.DateTimeFormat("ja-u-ca-japanese", {calendar: "islamic"}).resolvedOptions(); return [o.locale, o.calendar].join()`, "ja,gregory"},
		{"default locale", `const o = new Intl.DateTimeFormat().resolvedOptions(); return [o.locale, typeof o.timeZone].join()`, "en-US,string"},
		{"style and component", `return new Intl.DateTimeFormat("en", {dateStyle: "short", hour: "numeric"})`, "!TypeError"},
		{"invalid time", `return new Intl.DateTimeFormat().format(NaN)`, "!RangeError: Invalid time value"},
		{"time clip", `return new Intl.DateTimeFormat().format(8.64e15 + 1)`, "!RangeError: Invalid time value"},
		{"bad zone", `return new Intl.DateTimeFormat("en", {timeZone: "Mars/Olympus"})`, "!RangeError"},
		{"bad calendar", `return new Intl.DateTimeFormat("en", {calendar: "a"})`, "!RangeError"},
		{"bad fsd", `return new Intl.DateTimeFormat("en", {fractionalSecondDigits: 4})`, "!RangeError"},
		{"range missing", `return new Intl.DateTimeFormat().formatRange(0)`, "!TypeError"},
		{"option order", `const log = []; const o = new Proxy({}, {get(t, k) { log.push(String(k)); return undefined }}); new Intl.DateTimeFormat("en", o); return log.join()`,
			"localeMatcher,calendar,numberingSystem,hour12,hourCycle,timeZone,weekday,era,year,month,day,dayPeriod,hour,minute,second,fractionalSecondDigits,timeZoneName,formatMatcher,dateStyle,timeStyle"},
	})
}

// intlTestDTF returns the internal of a DateTimeFormat made by a script
// expression.
func intlTestDTF(t testing.TB, r *Realm, expr string) *intlDateTimeFormat {
	m, err := syntax.ParseModule("t.js", "export const f = "+expr, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	v, _ := env.GetBindingValue("f")
	return v.AsObject().internal.(*intlDateTimeFormat)
}

// TestIntlDateTimeFormatAllocs: format allocates its result only, one
// allocation for a short ASCII or UTF-16 result.
func TestIntlDateTimeFormatAllocs(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true, TimeZone: time.UTC})
	for _, c := range []struct{ name, expr, want string }{
		{"tokyo", `new Intl.DateTimeFormat("en-US", {timeZone: "Asia/Tokyo", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", hourCycle: "h23"})`, "10/22/2026, 09"},
		{"styles", `new Intl.DateTimeFormat("en-US", {dateStyle: "medium", timeStyle: "short", timeZone: "UTC"})`, "Oct 22, 2026, 12:00 AM"},
		{"zh", `new Intl.DateTimeFormat("zh-Hans", {dateStyle: "full", timeStyle: "short", timeZone: "UTC"})`, "2026年10月22日星期四 00:00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := intlTestDTF(t, r, c.expr)
			tv := float64(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).UnixMilli())
			assert.Equal(t, c.want, f.format(tv).GoString())
			n := testing.AllocsPerRun(100, func() { f.format(tv) })
			assert.LessOrEqual(t, n, 1.0)
		})
	}
}

// TestIntlDateDataLazy: no locale's calendar data decodes before a
// formatter of that locale needs it.
func TestIntlDateDataLazy(t *testing.T) {
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	_, err := runScriptValue(t, r, `new Intl.DateTimeFormat("it", {timeZone: "UTC"}).format(0)`)
	require.NoError(t, err)
	it, _ := intlDataIndex("it")
	assert.NotNil(t, intlDateTables[it])
	// No test of this package formats Russian before this one.
	ru, _ := intlDataIndex("ru")
	if intlDateTables[ru] == nil {
		_, err = runScriptValue(t, r, `new Intl.DateTimeFormat("ru", {timeZone: "UTC"})`)
		require.NoError(t, err)
		assert.NotNil(t, intlDateTables[ru])
	}
}

func BenchmarkIntlDateFormat(b *testing.B) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true, TimeZone: time.UTC})
	tv := float64(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).UnixMilli())
	for _, c := range []struct{ name, expr string }{
		{"tokyo", `new Intl.DateTimeFormat("en-US", {timeZone: "Asia/Tokyo", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", hourCycle: "h23"})`},
		{"styles", `new Intl.DateTimeFormat("en-US", {dateStyle: "medium", timeStyle: "short", timeZone: "UTC"})`},
		{"zh-full", `new Intl.DateTimeFormat("zh-Hans", {dateStyle: "full", timeStyle: "medium", timeZone: "Asia/Shanghai"})`},
	} {
		f := intlTestDTF(b, r, c.expr)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := range b.N {
				f.format(tv + float64(i%1000)*60000)
			}
		})
	}
}

func BenchmarkIntlDateConstruct(b *testing.B) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true, TimeZone: time.UTC})
	opts := intlTestObject(b, r, `({timeZone: "Asia/Tokyo", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", hourCycle: "h23"})`)
	styles := intlTestObject(b, r, `({dateStyle: "long", timeStyle: "short"})`)
	for _, c := range []struct {
		name   string
		locale string
		opts   Value
	}{{"tokyo", "en-US", opts}, {"styles", "de", styles}} {
		loc := StringValue(FromGoString(c.locale))
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := r.intlNewDateTimeFormat(loc, c.opts, intlRequireAny, intlDefaultsDate); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func intlTestObject(t testing.TB, r *Realm, expr string) Value {
	m, err := syntax.ParseModule("t.js", "export const o = "+expr, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	v, _ := env.GetBindingValue("o")
	return v
}

// TestIntlDateTimeFormatMemoryLimit: the strings DateTimeFormat returns
// (format through the bound function, formatToParts' values, formatRange,
// formatRangeToParts) and the data each formatter keeps are charged to the
// realm's memory account, so a script that keeps them stops at the limit:
// the kept strings of the format case take ~35 MB with their headers, the
// array holding them ~10 MB.
func TestIntlDateTimeFormatMemoryLimit(t *testing.T) {
	const full = `const f = new Intl.DateTimeFormat("en", {dateStyle: "full", timeStyle: "full", timeZone: "Asia/Tokyo"}); const keep = [];`
	for name, body := range map[string]string{
		"format":             full + `for (let i = 0; i < 300000; i++) keep.push(f.format(i * 60000)); return keep.length`,
		"formatToParts":      full + `for (let i = 0; i < 300000; i++) keep.push(f.formatToParts(i * 60000)); return keep.length`,
		"formatRange":        full + `for (let i = 0; i < 300000; i++) keep.push(f.formatRange(i * 60000, i * 60000 + 3 * 86400000)); return keep.length`,
		"formatRangeToParts": full + `for (let i = 0; i < 300000; i++) keep.push(f.formatRangeToParts(i * 60000, i * 60000 + 3600000)); return keep.length`,
		"construct":          `const keep = []; for (let i = 0; i < 60000; i++) keep.push(new Intl.DateTimeFormat("en-u-nu-fullwide", {timeZone: "America/Argentina/ComodRivadavia"})); return keep.length`,
	} {
		t.Run(name, func(t *testing.T) {
			hit, res, allocated := intlMemLimitRun(t, 16<<20, body)
			assert.True(t, hit, "under a 16 MB limit: result %s, charged %d bytes", res, allocated)
		})
	}
	// Each formatted string is charged with its header.
	setup := full + `const n = 1000;`
	_, _, base := intlMemLimitRun(t, 1<<40, setup+`return 0`)
	_, res, all := intlMemLimitRun(t, 1<<40, setup+`for (let i = 0; i < n; i++) keep.push(f.format(i * 60000)); return keep[0].length`)
	assert.GreaterOrEqual(t, (all-base)/1000, int64(stringHeaderSize+60), "charged per format, of %s characters", res)
}

// TestIntlPatternCacheStopsCounting: once the pattern caches' budget is
// spent, no cache stores more and the counter stops, however many
// patterns a long-running process compiles.
func TestIntlPatternCacheStopsCounting(t *testing.T) {
	d := intlDate(0)
	d.compile(nil, "y") // the cache holds something
	saved := intlDateCacheUsed.Load()
	defer intlDateCacheUsed.Store(saved)
	intlDateCacheUsed.Store(intlDateCacheBudget) // a long-running process's misses
	before := 0
	d.compiled.Range(func(any, any) bool { before++; return true })
	for i := range 100 {
		d.compile(nil, fmt.Sprintf("'miss%d' y", i))
	}
	after := 0
	d.compiled.Range(func(any, any) bool { after++; return true })
	assert.Equal(t, before, after, "a spent budget stores no pattern")
	assert.Equal(t, int64(intlDateCacheBudget), intlDateCacheUsed.Load(), "a spent budget's counter does not move")
	for i := range 100 {
		d.compiled.Delete(fmt.Sprintf("'miss%d' y", i))
	}
}

// TestIntlRegionalLocales: the regional locales of the data locales'
// languages format with their own CLDR data, inherited by CLDR's parent
// locales (en-GB from en-001, pt-AO from pt-PT), and resolve to themselves;
// a locale whose data is its parent's (en-AG, en-001's but for its
// currency symbol) too. The expected values are Node 24.22's.
func TestIntlRegionalLocales(t *testing.T) {
	const d = `const d = Date.UTC(2026, 9, 11, 14, 5);`
	runProtoCases(t, []protoCase{
		{"en-GB date", d + `const f = new Intl.DateTimeFormat("en-GB", {dateStyle: "short", timeZone: "UTC"}); return f.resolvedOptions().locale + " " + f.format(d)`, `en-GB 11/10/2026`},
		{"en-AU date", d + `return new Intl.DateTimeFormat("en-AU", {dateStyle: "short", timeZone: "UTC"}).format(d)`, `11/10/26`},
		{"en-CA en-NZ en-IE en-ZA dates", d + `return ["en-CA", "en-NZ", "en-IE", "en-ZA"].map(l => new Intl.DateTimeFormat(l, {dateStyle: "short", timeZone: "UTC"}).format(d)).join(" | ")`, `2026-10-11 | 11/10/2026 | 11/10/2026 | 2026/10/11`},
		{"en-IN grouping", `return new Intl.NumberFormat("en-IN").format(1234567.891)`, `12,34,567.891`},
		{"en-IN currency", `return new Intl.NumberFormat("en-IN", {style: "currency", currency: "INR"}).format(1234567.891)`, "₹12,34,567.89"},
		{"en-GB toLocaleDateString", d + `return new Date(d).toLocaleDateString("en-GB", {timeZone: "UTC"})`, `11/10/2026`},
		{"en-GB time zone", d + `return new Intl.DateTimeFormat("en-GB", {timeZone: "America/New_York", timeZoneName: "short"}).format(d)`, `11/10/2026, GMT-4`},
		{"en-AU time zone", d + `return new Intl.DateTimeFormat("en-AU", {timeZone: "Australia/Sydney", timeZoneName: "short", hour: "numeric", minute: "numeric"}).format(d)`, `1:05 am AEDT`},
		{"en-AG", d + `const f = new Intl.DateTimeFormat("en-AG", {dateStyle: "medium", timeZone: "UTC"}); return f.resolvedOptions().locale + " " + f.format(d) + " " + new Intl.NumberFormat("en-AG", {style: "currency", currency: "XCD"}).format(5)`, `en-AG 11 Oct 2026 $5.00`},
		{"de-CH", `return new Intl.NumberFormat("de-CH").format(1234567.891) + " " + new Intl.NumberFormat("de-CH", {style: "currency", currency: "CHF"}).format(1234.5)`, "1'234'567.891 CHF\u00a01'234.50"},
		{"de-AT", `return new Intl.DateTimeFormat("de-AT", {dateStyle: "long", timeZone: "UTC"}).format(Date.UTC(2026, 0, 11)) + " " + new Intl.NumberFormat("de-AT").format(1234.5)`, "11. Jänner 2026 1\u00a0234,5"},
		{"zh-HK", d + `return new Intl.DateTimeFormat("zh-HK", {dateStyle: "short", timeStyle: "short", timeZone: "UTC"}).format(d) + " " + new Intl.NumberFormat("zh-HK", {style: "unit", unit: "meter", unitDisplay: "long"}).format(5)`, "11/10/2026\u2009下午2:05 5 米"},
		{"zh-SG", d + `return new Intl.DateTimeFormat("zh-SG", {dateStyle: "short", timeZone: "UTC"}).format(d)`, `11/10/26`},
		{"fr-CA", d + `return new Intl.DateTimeFormat("fr-CA", {dateStyle: "short", timeZone: "UTC"}).format(d) + " " + new Intl.NumberFormat("fr-CA").format(1234.5)`, "2026-10-11 1\u00a0234,5"},
		{"fr-CH", d + `return new Intl.NumberFormat("fr-CH").format(1234.5) + " " + new Intl.DateTimeFormat("fr-CH", {dateStyle: "short", timeZone: "UTC"}).format(d)`, `1'234,5 11.10.26`},
		{"pt-PT", d + `return new Intl.DateTimeFormat("pt-PT", {dateStyle: "long", timeZone: "UTC"}).format(d) + " " + new Intl.NumberFormat("pt-PT").format(1234.5)`, `11 de outubro de 2026 1234,5`},
		{"es-MX es-419", d + `return new Intl.NumberFormat("es-MX").format(1234.5) + " " + new Intl.NumberFormat("es-419").format(1234.5) + " " + new Intl.DateTimeFormat("es-MX", {dateStyle: "medium", timeZone: "UTC"}).format(d)`, `1,234.5 1,234.5 11 oct 2026`},
		// en-150's euro pattern comes with en-150's separators in en-NL.
		{"en-150 euro", `return ["en-150", "en-NL"].map(l => new Intl.NumberFormat(l, {style: "currency", currency: "EUR"}).format(-1234.5)).join(" | ")`, "-€1,234.50 | -€1,234.50"},
		// de-LU's franc has separators of its own and de-LU's pattern.
		{"de-LU franc", `return new Intl.NumberFormat("de-LU", {style: "currency", currency: "LUF"}).format(1234.5)`, "1,235\u00a0F"},
		// The generic names take the locale's region: New York is not the
		// zone of Jamaica's Eastern Time.
		{"en-JM generic", d + `return new Intl.DateTimeFormat("en-JM", {hour: "numeric", timeZoneName: "longGeneric", timeZone: "America/New_York"}).format(d)`, `10 am Eastern Time (United States)`},
		// ICU's patterns where they are not CLDR 48.0's: fr-CH's group,
		// es-419's (and es-AR's) Hv.
		{"ICU's data", `return new Intl.NumberFormat("fr-CH").format(1234.5) + " " + new Intl.DateTimeFormat("es-AR", {hour: "numeric", hourCycle: "h23", timeZoneName: "shortGeneric", timeZone: "Asia/Tokyo"}).format(0)`, "1'234,5 09 hora de Japón"},
		// A currency without an override of its own takes the default
		// currency's, as in ICU: en-NL's euro pattern, pt-CV's escudo
		// decimal separator.
		{"default currency", `return ["USD", "EUR"].map(c => new Intl.NumberFormat("en-NL", {style: "currency", currency: c, currencySign: "accounting"}).format(-1234.5)).join(" | ") + " " + new Intl.NumberFormat("pt-CV", {style: "currency", currency: "EUR"}).format(1234.5)`, "-US$1,234.50 | -€1,234.50 1234$50\u00a0€"},
		// CLDR's "∅∅∅" is no name: ja's short generic name of Japan.
		{"ja shortGeneric", `return new Intl.DateTimeFormat("ja", {timeZone: "Asia/Tokyo", timeZoneName: "shortGeneric"}).format(0)`, `1970/1/1 JST`},
	})
}

// TestIntlRegionalChains: inheritance over more than one
// level (en-NL → en-150 → en-001 → en, es-AR → es-419 → es, pt-AO →
// pt-PT → pt, zh-MO → zh-Hant-HK → zh-Hant), parentLocales overrides
// (hi-Latn → en-IN) and -u- keywords on a regional locale. The expected
// values are Node 24.22's.
func TestIntlRegionalChains(t *testing.T) {
	const d = `const d = Date.UTC(2026, 9, 11, 14, 5, 9);`
	runProtoCases(t, []protoCase{
		{"en-NL", d + `return [new Intl.NumberFormat("en-NL", {style: "currency", currency: "EUR"}).format(-1234.5), new Intl.DateTimeFormat("en-NL", {dateStyle: "short", timeStyle: "short", timeZone: "UTC"}).format(d)].join(" | ")`, "-€1,234.50 | 11/10/2026, 14:05"},
		{"es-AR", d + `return [new Intl.NumberFormat("es-AR").format(1234567.5), new Intl.NumberFormat("es-AR", {style: "currency", currency: "ARS"}).format(-5), new Intl.DateTimeFormat("es-AR", {dateStyle: "medium", timeStyle: "short", timeZone: "UTC"}).format(d)].join(" | ")`, "1.234.567,5 | -$\u00a05,00 | 11 oct 2026, 2:05 p. m."},
		{"pt-AO", d + `return [new Intl.NumberFormat("pt-AO").format(1234567.5), new Intl.NumberFormat("pt-AO", {style: "currency", currency: "AOA"}).format(5), new Intl.DateTimeFormat("pt-AO", {dateStyle: "short", timeZone: "UTC"}).format(d), new Intl.PluralRules("pt-AO").select(0)].join(" | ")`, "1\u00a0234\u00a0567,5 | 5,00\u00a0Kz | 11/10/26 | one"},
		{"zh-MO", d + `return [new Intl.DateTimeFormat("zh-MO", {dateStyle: "short", timeStyle: "short", timeZone: "UTC"}).format(d), new Intl.NumberFormat("zh-MO", {style: "currency", currency: "MOP"}).format(5)].join(" | ")`, "11/10/2026\u2009下午2:05 | MOP$5.00"},
		{"hi-Latn", d + `return [new Intl.NumberFormat("hi-Latn").format(1234567.5), new Intl.DateTimeFormat("hi-Latn", {dateStyle: "medium", timeZone: "UTC"}).format(d), new Intl.DateTimeFormat("hi-Latn").resolvedOptions().locale].join(" | ")`, "12,34,567.5 | 11 Oct, 2026 | hi-Latn"},
		{"keywords", d + `return ["en-GB-u-nu-arab", "en-GB-u-hc-h11", "de-CH-u-nu-latn-hc-h12", "zh-HK-u-ca-gregory"].map(t => { const f = new Intl.DateTimeFormat(t, {hour: "numeric", minute: "numeric", day: "numeric", timeZone: "UTC"}); return new Intl.NumberFormat(t).format(1234.5) + " " + f.format(d) + " " + f.resolvedOptions().locale }).join(" | ")`, "١٬٢٣٤٫٥ ١١, ١٤:٠٥ en-GB-u-nu-arab | 1,234.5 11, 2:05 pm en-GB-u-hc-h11 | 1'234.5 11, 2:05 PM de-CH-u-hc-h12-nu-latn | 1,234.5 11日\u2009下午2:05 zh-HK-u-ca-gregory"},
	})
}

package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntlLocaleInfo checks the Intl.Locale info methods against Node 24
// (ICU 78.3, CLDR 48) where moejs's data agrees: getCalendars is
// ["gregory"] (DateTimeFormat has no other calendar) and getNumberingSystems
// is latn for a locale without number data (ICU gives arab for ar-EG).
func TestIntlLocaleInfo(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"calendars", `return ["ja-JP", "th", "en-u-ca-japanese", "fa-IR"].map(l => new Intl.Locale(l).getCalendars().join()).join("|")`, "gregory|gregory|japanese|gregory"},
		{"collations", `return ["en", "de", "zh-TW", "zh-Hant", "ja-JP", "es-ES", "yue", "ln", "en-u-co-phonebk"].map(l => new Intl.Locale(l).getCollations().join()).join("|")`,
			"emoji,eor|emoji,eor,phonebk|emoji,eor,pinyin,stroke,unihan,zhuyin|emoji,eor,pinyin,stroke,unihan,zhuyin|emoji,eor,unihan|emoji,eor,trad|emoji,eor,pinyin,stroke,unihan,zhuyin|emoji,eor,phonetic|phonebk"},
		{"hour cycles", `return ["en-US", "en-GB", "ja", "zh-TW", "en-IL", "ar-EG", "und", "en-u-hc-h23", "en-u-rg-gbzzzz", "en-u-sd-usca"].map(l => new Intl.Locale(l).getHourCycles().join()).join()`, "h12,h23,h23,h12,h23,h12,h12,h23,h23,h12"},
		{"numbering systems", `return ["en", "zh-TW", "ar-EG", "en-u-nu-arab"].map(l => new Intl.Locale(l).getNumberingSystems().join()).join()`, "latn,latn,latn,arab"},
		{"text info", `return ["en", "ar", "he-IL", "und-Arab", "ff-Adlm", "mn-Mong", "zh"].map(l => new Intl.Locale(l).getTextInfo().direction).join() + JSON.stringify(new Intl.Locale("en").getTextInfo())`, `ltr,rtl,rtl,rtl,rtl,ltr,ltr{"direction":"ltr"}`},
		{"week info", `return ["en-US", "en-GB", "ar-EG", "fa-IR", "hi-IN", "ps", "dv", "zh", "de-u-fw-sun", "en-u-rg-gbzzzz", "en-u-fw-xyz"].map(l => JSON.stringify(new Intl.Locale(l).getWeekInfo())).join("|")`,
			`{"firstDay":7,"weekend":[6,7]}|{"firstDay":1,"weekend":[6,7]}|{"firstDay":6,"weekend":[5,6]}|{"firstDay":6,"weekend":[5]}|{"firstDay":7,"weekend":[7]}|{"firstDay":6,"weekend":[4,5]}|{"firstDay":5,"weekend":[6,7]}|{"firstDay":1,"weekend":[6,7]}|{"firstDay":7,"weekend":[6,7]}|{"firstDay":1,"weekend":[6,7]}|{"firstDay":7,"weekend":[6,7]}`},
		{"time zones without region", `return [new Intl.Locale("en").getTimeZones(), Array.isArray(new Intl.Locale("en-US").getTimeZones())].join()`, ",true"},
		{"receivers", `return Intl.Locale.prototype.getWeekInfo.call({})`, "!TypeError: Method Intl.Locale.prototype.getWeekInfo called on incompatible receiver"},
		{"lengths", `return ["getCalendars", "getCollations", "getHourCycles", "getNumberingSystems", "getTimeZones", "getTextInfo", "getWeekInfo"].map(m => Intl.Locale.prototype[m].length + Intl.Locale.prototype[m].name).join()`,
			"0getCalendars,0getCollations,0getHourCycles,0getNumberingSystems,0getTimeZones,0getTextInfo,0getWeekInfo"},
		{"fresh arrays", `const l = new Intl.Locale("de"); return l.getCollations() !== l.getCollations()`, "true"},
		{"firstDayOfWeek true", `return [new Intl.Locale("en", {firstDayOfWeek: true}).firstDayOfWeek, new Intl.Locale("en", {firstDayOfWeek: true}).toString()].join()`, ",en-u-fw"},
	})
}

// TestIntlSupportedValuesOf checks the lists that do not depend on the
// constructors' data.
func TestIntlSupportedValuesOf(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"calendar", `return Intl.supportedValuesOf("calendar").join()`, "gregory"},
		{"collation", `return Intl.supportedValuesOf("collation").join()`, "compat,dict,emoji,eor,phonebk,phonetic,pinyin,searchjl,stroke,trad,unihan,zhuyin"},
		{"numbering systems", `const l = Intl.supportedValuesOf("numberingSystem"); return [l.includes("latn"), l.includes("arab"), l.includes("hanidec"), l.includes("roman"), l.join() === [...l].sort().join()].join()`, "true,true,true,false,true"},
		{"key", `return Intl.supportedValuesOf("calendars")`, "!RangeError"},
		{"key string", `return Intl.supportedValuesOf({toString() { return "calendar" }}).join()`, "gregory"},
	})
}

// TestIntlDefaultLocale checks RealmOptions.Locale and Realm.SetLocale.
func TestIntlDefaultLocale(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "en-US"}, {"en-US", "en-US"}, {"zh-cn", "zh-CN"}, {"de-DE-u-co-phonebk", "de-DE"},
		{"en_US", "en-US"}, {"iw", "he"}, {"x", "en-US"}, {"sv-SE", "sv-SE"},
	} {
		for _, shared := range []bool{false, true} {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared, Locale: c.in})
			assert.Equal(t, c.want, r.Locale(), c.in)
		}
	}
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared, Locale: "de-CH"})
		body := `return [new Intl.Segmenter().resolvedOptions().locale, Intl.Segmenter.supportedLocalesOf("de-CH").join()].join()`
		assert.Equal(t, "de-CH,de-CH", evalIn(t, r, body))
		r.SetLocale("")
		assert.Equal(t, "en-US", r.Locale())
		assert.Equal(t, "en-US", evalIn(t, r, `return new Intl.Segmenter().resolvedOptions().locale`))
	}
}

// evalIn runs a function body in a realm and returns its result as
// evalProtoBody does.
func evalIn(t *testing.T, r *Realm, body string) string {
	t.Helper()
	m, err := syntax.ParseModule("t.js", "export function f() {\n"+body+"\n}", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	res, err := r.Call(fn, Undefined(), nil)
	if err != nil {
		var exc *Exception
		if asException(err, &exc) {
			return "!" + errorDisplayString(exc.Value)
		}
		return "!" + err.Error()
	}
	return fmt.Sprintf("%v", r.ToGo(res))
}

// TestIntlMemoryLimitToLocale checks that the strings of the toLocaleString
// methods and of the Locale info methods and supportedValuesOf are charged:
// a script that keeps n results is charged at least their characters.
func TestIntlMemoryLimitToLocale(t *testing.T) {
	for _, c := range []struct {
		name, expr string
		n          int64 // results kept
		chars      int64 // characters of one result, at least
	}{
		{"number", `(1234567.5 + i).toLocaleString()`, 20000, 11},
		{"number locale", `(1234567.5 + i).toLocaleString("de")`, 20000, 11},
		{"bigint", `(12345678901234567890n + BigInt(i)).toLocaleString()`, 20000, 26},
		{"date", `new Date(1e12 + i).toLocaleString()`, 20000, 20},
		{"date options", `new Date(1e12 + i).toLocaleDateString("ja", {dateStyle: "full", timeZone: "UTC"})`, 20000, 12},
		{"supportedValuesOf", `Intl.supportedValuesOf("timeZone")`, 50, 400 * 8},
		{"getTimeZones", `new Intl.Locale("en-US").getTimeZones()`, 1000, 29 * 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			loop := `const keep = []; for (let i = 0; i < ` + fmt.Sprint(c.n) + `; i++) keep.push(`
			_, _, empty := intlMemLimitRun(t, 1<<40, loop+`""); return keep.length`)
			_, res, got := intlMemLimitRun(t, 1<<40, loop+c.expr+`); return keep.length`)
			assert.Equal(t, fmt.Sprint(c.n), res)
			assert.GreaterOrEqual(t, got-empty, c.n*c.chars, "charged %d bytes beyond the array", got-empty)
		})
	}
}

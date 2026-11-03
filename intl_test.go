package moejs_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntlIssueExample runs the example of GitHub issue #4 (a TypeScript
// agent harness counting what users see, choosing terms by script and
// formatting money and Tokyo dates) and compares it with Node 24.22's output.
func TestIntlIssueExample(t *testing.T) {
	s, err := moejs.CompileScript("intl.js", `
const segmenter = new Intl.Segmenter("zh-Hans", { granularity: "grapheme" });
const result = {
  graphemes: [...segmenter.segment("👨‍👩‍👧‍👦é")].length,
  script: new Intl.Locale("zh-CN").maximize().script,
  money: new Intl.NumberFormat("ja-JP", {
    style: "currency", currency: "JPY",
  }).format(1250),
  parts: new Intl.DateTimeFormat("en-US", {
    timeZone: "Asia/Tokyo", year: "numeric", month: "2-digit",
    day: "2-digit", hour: "2-digit", hourCycle: "h23",
  }).formatToParts(new Date("2026-10-22T00:00:00Z")),
};
JSON.stringify(result);`)
	require.NoError(t, err)
	const node = `{"graphemes":2,"script":"Hans","money":"￥1,250","parts":[{"type":"month","value":"10"},{"type":"literal","value":"/"},{"type":"day","value":"22"},{"type":"literal","value":"/"},{"type":"year","value":"2026"},{"type":"literal","value":", "},{"type":"hour","value":"09"}]}`
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
		v, err := rt.RunScript(s)
		require.NoError(t, err)
		out, err := rt.ToGo(v)
		require.NoError(t, err)
		assert.Equal(t, node, out)
	}
}

// TestIntlConcurrent formats in every data locale from runtimes on many
// goroutines at once, shared and mutable, so the first use of each locale's
// tables, of the time zones, of the default formatters and of the shared
// strings of one-code-point segments races (run it with -race), and checks
// that every runtime gets the same results.
func TestIntlConcurrent(t *testing.T) {
	s, err := moejs.CompileScript("intl.js", `
const locales = ["en", "zh-Hans", "zh-Hant", "ja", "ko", "de", "fr", "es", "pt", "ru", "it", "sv"];
const d = new Date(Date.UTC(2026, 2, 29, 1, 30, 15, 250));
const out = [];
for (const l of locales) {
  out.push(
    new Intl.NumberFormat(l, {style: "currency", currency: "EUR"}).format(-1234567.891),
    new Intl.NumberFormat(l, {notation: "compact", compactDisplay: "long"}).format(12345),
    new Intl.NumberFormat(l, {style: "unit", unit: "kilometer-per-hour", unitDisplay: "long"}).format(2),
    new Intl.PluralRules(l, {type: "ordinal"}).select(22),
    new Intl.DateTimeFormat(l, {dateStyle: "full", timeStyle: "long", timeZone: "Europe/Berlin"}).format(d),
    JSON.stringify(new Intl.DateTimeFormat(l, {timeZone: "America/New_York", hour: "numeric", dayPeriod: "short"}).formatToParts(d)),
    new Intl.DateTimeFormat(l, {timeZone: "Asia/Tokyo", timeZoneName: "long"}).formatRange(d, d.getTime() + 86400e3),
    (1234.5).toLocaleString(), (12n ** 20n).toLocaleString(l), d.toLocaleString(), d.toLocaleDateString(l), [1.5, d].toLocaleString(l),
  );
}
const text = "你好，世界！日本語のテキスト。한국어 문장. Emoji: 👍🏽 🇯🇵 😀 🦀 ü é ñ Ω.";
for (const granularity of ["grapheme", "word", "sentence"]) {
  const counts = {}, seen = new Map();
  for (const {segment} of new Intl.Segmenter("en", {granularity}).segment(text)) {
    counts[segment] = (counts[segment] || 0) + 1;
    seen.set(segment, segment.length);
  }
  out.push(JSON.stringify(counts), JSON.stringify([...seen]));
}
out.join("\n");`)
	require.NoError(t, err)
	const n = 16
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: i%2 == 1, TimeZone: time.UTC})
			v, err := rt.RunScript(s)
			if err != nil {
				results[i] = fmt.Sprint("error: ", err)
				return
			}
			out, _ := rt.ToGo(v)
			results[i], _ = out.(string)
		}()
	}
	wg.Wait()
	require.NotEmpty(t, results[0])
	require.NotContains(t, results[0], "error:")
	for i := 1; i < n; i++ {
		assert.Equal(t, results[0], results[i], "runtime %d", i)
	}
}

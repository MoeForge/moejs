package engine

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/require"
)

// intlNumberNodeJS computes in moejs what testdata/intl_number_node.js
// computes in Node, from the same inputs.
const intlNumberNodeJS = `
function decode(v) {
  const k = v.slice(0, 2), s = v.slice(2);
  if (k === "n:") return Number(s);
  if (k === "b:") return BigInt(s);
  return s;
}
function run(fn) {
  try {
    return fn();
  } catch (e) {
    return "!" + e.constructor.name;
  }
}
function parts(ps) {
  return ps.map(p => p.source ? [p.type, p.value, p.source] : [p.type, p.value]);
}
const {locales, regional, nuLocales, values, partValues, rangePairs, configs, rangeConfigs, fullConfigs, extraConfigs, shortValues, allLocales,
  mainLocales, pluralLocales, pluralValues, pluralRanges} = input;
const out = {};
out.format = configs.map((c, ci) => allLocales.slice(0, ci < extraConfigs ? allLocales.length : mainLocales).map(l => {
  const nf = run(() => new Intl.NumberFormat(l, c));
  if (typeof nf === "string") return nf;
  const vs = ci < fullConfigs ? values : shortValues;
  const ps = ci < fullConfigs ? partValues : partValues.slice(0, 2);
  return {
    resolved: run(() => JSON.stringify(nf.resolvedOptions())),
    format: vs.map(v => run(() => nf.format(decode(v)))),
    parts: ps.map(v => run(() => parts(nf.formatToParts(decode(v))))),
  };
}));
out.range = rangeConfigs.map(c => [...locales, ...regional, ...nuLocales.slice(0, 2)].map(l => {
  const nf = new Intl.NumberFormat(l, c);
  return rangePairs.map(([x, y]) => run(() => [nf.formatRange(decode(x), decode(y)), parts(nf.formatRangeToParts(decode(x), decode(y)))]));
}));
out.plural = pluralLocales.map(l => ["cardinal", "ordinal"].map(type => {
  const pr = new Intl.PluralRules(l, {type});
  return {
    locale: pr.resolvedOptions().locale,
    categories: pr.resolvedOptions().pluralCategories.join(),
    select: pluralValues.map(v => run(() => pr.select(decode(v)))),
    range: pluralRanges.map(([x, y]) => run(() => pr.selectRange(decode(x), decode(y)))),
  };
}));
out.pluralOptions = [{minimumFractionDigits: 2}, {maximumSignificantDigits: 1}, {roundingMode: "floor", maximumFractionDigits: 0}].map(o =>
  ["en", "fr", "ru", "ar", "pl", "cy"].map(l => pluralValues.map(v => run(() => new Intl.PluralRules(l, o).select(decode(v))))));
return JSON.stringify(out);
`

// intlNodeFixture is testdata/intl_number_node.json.gz.
type intlNodeFixture struct {
	Node, ICU, CLDR              string
	Locales, Regional, Fallbacks []string
	NuLocales, AllLocales        []string
	Values, PartValues           []string
	ShortValues                  []string
	RangePairs                   [][2]string
	Configs, RangeConfigs        []json.RawMessage
	FullConfigs, ExtraConfigs    int
	MainLocales                  int
	PluralLocales, PluralValues  []string
	PluralRanges                 [][2]string
	intlNodeResults
}

type intlNodeResults struct {
	Format [][]json.RawMessage
	Range  [][][]json.RawMessage
	Plural [][]struct {
		Locale, Categories string
		Select, Range      []string
	}
	PluralOptions [][][]string
}

// intlNumberNodeDiff is an allowed difference from Node: a predicate on
// the case's description and what moejs and Node give, and the reason.
type intlNumberNodeDiff struct {
	match  func(what, got, want string) bool
	reason string
}

// intlNumberNodeDiffs lists every allowed difference from Node with its
// reason.
var intlNumberNodeDiffs = []intlNumberNodeDiff{
	{func(what, got, want string) bool {
		return strings.Contains(what, `"currencySign":"accounting","signDisplay":"never"} resolvedOptions`) &&
			strings.Replace(got, `"currencySign":"accounting"`, `"currencySign":"standard"`, 1) == want
	}, `V8 reads resolvedOptions from ICU's skeleton, which cannot say accounting with signDisplay "never"`},
	{func(what, got, want string) bool {
		return strings.HasPrefix(what, "en-IN ") && strings.Contains(what, `"currencySign":"accounting","signDisplay":"never"} format`) &&
			strings.Contains(want, ",") && strings.ReplaceAll(got, ",", "") == strings.ReplaceAll(want, ",", "")
	}, `V8 formats accounting with signDisplay "never" with the standard currency pattern, which in en-IN has the Indian grouping and its accounting pattern (en's) does not`},
	{func(what, got, want string) bool {
		return strings.HasPrefix(what, "PluralRules") && strings.HasSuffix(what, " b:1000000") && want == "!TypeError"
	}, "V8 13.6 converts the value of select with ToNumber, which throws on a BigInt; ECMA-402 uses ToIntlMathematicalValue"},
	{func(what, got, want string) bool {
		return strings.HasPrefix(what, "PluralRules sh ") && strings.HasSuffix(what, " locale") && got == "sr-Latn" && want == "sr"
	}, "PluralRules' available locales are CLDR's, sh being sr-Latn, which has locale data; V8's are those plurals.xml lists, which resolve sr-Latn to sr"},
	{func(what, got, want string) bool {
		return strings.HasPrefix(what, "PluralRules") && strings.HasSuffix(what, "selectRange n:1 n:1")
	}, "selectRange of numbers that format alike is the start's category (ResolvePluralRange step 4); V8 resolves every range through ICU's plural ranges"},
	{func(what, got, want string) bool {
		return strings.Contains(what, `"roundingIncrement":25`) && strings.HasSuffix(what, "format n:1.7976931348623157e308")
	}, "ICU rounds to an increment by dividing and multiplying with decNumber, which keeps 16 digits of 1.7976931348623157e308; the decimal core rounds exactly"},
	{func(what, got, want string) bool {
		if !strings.Contains(what, `"currencyDisplay":"name"`) {
			return false
		}
		for _, v := range []string{" n:1e21", " n:1.7976931348623157e308", " b:123456789012345678901234567890"} {
			if strings.HasSuffix(what, v) {
				return true
			}
		}
		return false
	}, "ICU's plural operand i keeps the 18 lowest integer digits (DecimalQuantity::toLong), so 1e21 is \"one\" in fr and pt; the decimal core's i is the whole integer part"},
}

func TestIntlNumberNode(t *testing.T) {
	f, err := os.Open("testdata/intl_number_node.json.gz")
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	data, err := io.ReadAll(zr)
	require.NoError(t, err)
	var fx intlNodeFixture
	require.NoError(t, json.Unmarshal(data, &fx))
	t.Logf("Node %s, ICU %s, CLDR %s", fx.Node, fx.ICU, fx.CLDR)

	input, err := json.Marshal(map[string]any{
		"locales": fx.Locales, "nuLocales": fx.NuLocales, "values": fx.Values, "partValues": fx.PartValues,
		"rangePairs": fx.RangePairs, "configs": fx.Configs, "rangeConfigs": fx.RangeConfigs, "fullConfigs": fx.FullConfigs,
		"regional": fx.Regional, "mainLocales": fx.MainLocales,
		"extraConfigs": fx.ExtraConfigs, "shortValues": fx.ShortValues, "allLocales": fx.AllLocales,
		"pluralLocales": fx.PluralLocales, "pluralValues": fx.PluralValues, "pluralRanges": fx.PluralRanges,
	})
	require.NoError(t, err)
	quoted, err := json.Marshal(string(input))
	require.NoError(t, err)
	body := "const input = JSON.parse(" + string(quoted) + ");\n" + intlNumberNodeJS
	m, err := syntax.ParseModule("t.js", "export function f() {\n"+body+"\n}", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(RealmOptions{})
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	res, err := r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	var got intlNodeResults
	require.NoError(t, json.Unmarshal([]byte(res.AsString().GoString()), &got))

	// MOEJS_INTL_NODE_DIFFS=1 logs the allowed differences too.
	listDiffs := os.Getenv("MOEJS_INTL_NODE_DIFFS") != ""
	var cases, same, allowed int
	reasons := map[string]int{}
	check := func(what, g, w string) {
		cases++
		if g == w {
			same++
			return
		}
		for _, d := range intlNumberNodeDiffs {
			if d.match(what, g, w) {
				allowed++
				reasons[d.reason]++
				if listDiffs {
					t.Logf("allowed %s:\n got %s\nwant %s", what, g, w)
				}
				return
			}
		}
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
	for ci, cfg := range fx.Configs {
		for li, wantRaw := range fx.Format[ci] {
			what := fmt.Sprintf("%s %s", fx.AllLocales[li], cfg)
			gotRaw := got.Format[ci][li]
			var w, g struct {
				Resolved string
				Format   []string
				Parts    []json.RawMessage
			}
			if json.Unmarshal(wantRaw, &w) != nil || json.Unmarshal(gotRaw, &g) != nil {
				check(what, string(gotRaw), string(wantRaw))
				continue
			}
			check(what+" resolvedOptions", g.Resolved, w.Resolved)
			vs := fx.Values
			if ci >= fx.FullConfigs {
				vs = fx.ShortValues
			}
			for vi, v := range vs {
				check(what+" format "+v, g.Format[vi], w.Format[vi])
			}
			for pi := range w.Parts {
				check(what+" formatToParts "+fx.PartValues[pi], string(g.Parts[pi]), string(w.Parts[pi]))
			}
		}
	}
	rangeLocales := append(append(append([]string(nil), fx.Locales...), fx.Regional...), fx.NuLocales[:2]...)
	for ci, cfg := range fx.RangeConfigs {
		for li, l := range rangeLocales {
			for pi, p := range fx.RangePairs {
				check(fmt.Sprintf("%s %s formatRange %s %s", l, cfg, p[0], p[1]), string(got.Range[ci][li][pi]), string(fx.Range[ci][li][pi]))
			}
		}
	}
	for li, l := range fx.PluralLocales {
		for ti, typ := range []string{"cardinal", "ordinal"} {
			g, w := got.Plural[li][ti], fx.Plural[li][ti]
			what := fmt.Sprintf("PluralRules %s %s", l, typ)
			check(what+" locale", g.Locale, w.Locale)
			check(what+" pluralCategories", g.Categories, w.Categories)
			for vi, v := range fx.PluralValues {
				check(what+" select "+v, g.Select[vi], w.Select[vi])
			}
			for pi, p := range fx.PluralRanges {
				check(what+" selectRange "+p[0]+" "+p[1], g.Range[pi], w.Range[pi])
			}
		}
	}
	for oi := range fx.PluralOptions {
		for li := range fx.PluralOptions[oi] {
			for vi, v := range fx.PluralValues {
				check(fmt.Sprintf("PluralRules options %d locale %d select %s", oi, li, v), got.PluralOptions[oi][li][vi], fx.PluralOptions[oi][li][vi])
			}
		}
	}
	t.Logf("%d cases: %d equal, %d allowed differences", cases, same, allowed)
	for reason, n := range reasons {
		t.Logf("  %d: %s", n, reason)
	}
}

package engine

import (
	"reflect"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntlShape(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"namespace", `return [typeof Intl, Object.prototype.toString.call(Intl), Object.getOwnPropertyNames(Intl), Object.getPrototypeOf(Intl) === Object.prototype].join("|")`, "object|[object Intl]|getCanonicalLocales,supportedValuesOf,NumberFormat,PluralRules,DateTimeFormat,Locale,Segmenter|true"},
		{"functions", `return [Intl.getCanonicalLocales.length, Intl.Locale.length, Intl.Locale.name, Intl.Segmenter.length, Intl.Segmenter.name, Intl.Segmenter.supportedLocalesOf.length].join()`, "1,1,Locale,0,Segmenter,1"},
		{"tags", `const s = new Intl.Segmenter().segment("a"); return [new Intl.Segmenter(), new Intl.Locale("en"), s, s[Symbol.iterator]()].map(o => Object.prototype.toString.call(o)).join()`, "[object Intl.Segmenter],[object Intl.Locale],[object Object],[object Segmenter String Iterator]"},
		{"iterator prototype", `const it = new Intl.Segmenter().segment("a")[Symbol.iterator](); return Object.getPrototypeOf(Object.getPrototypeOf(it)) === Object.getPrototypeOf(Object.getPrototypeOf([].values())) && it[Symbol.iterator]() === it`, "true"},
		{"segmenter call", `return Intl.Segmenter()`, "!TypeError: Constructor Intl.Segmenter requires 'new'"},
		{"locale call", `return Intl.Locale("en")`, "!TypeError: Constructor Intl.Locale requires 'new'"},
		{"segment receiver", `return Intl.Segmenter.prototype.segment.call(new Intl.Locale("en"), "a")`, "!TypeError: Method Intl.Segmenter.prototype.segment called on incompatible receiver"},
		{"containing receiver", `const s = new Intl.Segmenter().segment("a"); return Object.getPrototypeOf(s).containing.call(s[Symbol.iterator](), 0)`, "!TypeError"},
		{"next receiver", `const it = new Intl.Segmenter().segment("a")[Symbol.iterator](); return Object.getPrototypeOf(it).next.call(new Intl.Segmenter().segment("a"))`, "!TypeError"},
		{"locale receiver", `return Object.getOwnPropertyDescriptor(Intl.Locale.prototype, "language").get.call(new Intl.Segmenter())`, "!TypeError: Method Intl.Locale.prototype.language called on incompatible receiver"},
		{"subclass", `class S extends Intl.Segmenter {} class L extends Intl.Locale {} const s = new S("de"), l = new L("de"); return [s instanceof S, s.resolvedOptions().locale, l instanceof L, l.maximize() instanceof L, l.maximize() instanceof Intl.Locale].join()`, "true,de,true,false,true"},
	})
	runMutableProtoCases(t, []protoCase{
		{"binding", `const d = Object.getOwnPropertyDescriptor(globalThis, "Intl"); return [d.writable, d.enumerable, d.configurable].join()`, "true,false,true"},
		{"members", `const d = Object.getOwnPropertyDescriptor(Intl, "Segmenter"), g = Object.getOwnPropertyDescriptor(Intl.Locale.prototype, "baseName"), t = Object.getOwnPropertyDescriptor(Intl, Symbol.toStringTag); return [d.writable, d.enumerable, d.configurable, typeof g.get, g.set, g.get.name, t.value, t.writable, t.configurable].join()`, "true,false,true,function,,get baseName,Intl,false,true"},
		{"patched next", `const s = new Intl.Segmenter().segment("abc"), p = Object.getPrototypeOf(s[Symbol.iterator]()); p.next = function () { return {done: true} }; return [...s].length`, "0"},
		{"patched iterator", `const s = new Intl.Segmenter().segment("abc"); Object.getPrototypeOf(s)[Symbol.iterator] = function* () { yield 1 }; return [...s].join()`, "1"},
	})
}

func TestIntlSegmenter(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"graphemes", `return [...new Intl.Segmenter("zh-Hans", {granularity: "grapheme"}).segment("👨‍👩‍👧‍👦é")].map(s => s.segment).join("|")`, "👨‍👩‍👧‍👦|é"},
		{"data", `const t = "ab"; return JSON.stringify([...new Intl.Segmenter().segment(t)]) + [...new Intl.Segmenter().segment(t)].every(s => s.input === t)`, `[{"segment":"a","index":0,"input":"ab"},{"segment":"b","index":1,"input":"ab"}]true`},
		{"words", `return [...new Intl.Segmenter("en", {granularity: "word"}).segment("Hello, wörld 42!")].map(s => s.segment + ":" + s.isWordLike).join("|")`, "Hello:true|,:false| :false|wörld:true| :false|42:true|!:false"},
		{"sentences", `return [...new Intl.Segmenter("en", {granularity: "sentence"}).segment("One. Two? Three!")].map(s => s.segment).join("|")`, "One. |Two? |Three!"},
		{"rope", `let a = "x".repeat(40), b = "é".repeat(40); const s = a + b; return [[...new Intl.Segmenter().segment(s)].length, new Intl.Segmenter().segment(a + a).containing(41).index].join()`, "80,41"},
		{"empty", `return [...new Intl.Segmenter().segment("")].length + ":" + new Intl.Segmenter().segment("").containing(0)`, "0:undefined"},
		{"containing", `const s = new Intl.Segmenter("en", {granularity: "word"}).segment("ab cd"); return [s.containing(-1), s.containing(5), s.containing(Infinity), s.containing(4).segment, s.containing(1.9).index, s.containing("3").segment, s.containing().segment, s.containing(NaN).segment, s.containing(2).isWordLike].join()`, ",,,cd,0,cd,ab,ab,false"},
		{"segment string", `const s = new Intl.Segmenter(); return [...s.segment()].map(x => x.segment).join("") + [...s.segment(null)].length + [...s.segment({toString() { return "xy" }})].length`, "undefined42"},
		{"iterator done", `const it = new Intl.Segmenter().segment("a")[Symbol.iterator](); const a = it.next(), b = it.next(), c = it.next(); return [a.value.segment, a.done, b.value, b.done, c.done].join()`, "a,false,,true,true"},
		{"iterators", `const s = new Intl.Segmenter().segment("ab"), i = s[Symbol.iterator](), j = s[Symbol.iterator](); i.next(); return [i.next().value.segment, j.next().value.segment, [...s].length, [...s].length].join()`, "b,a,2,2"},
		{"next mixed with containing", `const s = new Intl.Segmenter().segment("abc"), it = s[Symbol.iterator](); it.next(); s.containing(2); return it.next().value.index`, "1"},
		{"resolvedOptions", `return [undefined, "zh-Hans", ["xx", "de-CH"], "zxx", "en-u-ca-gregory", "EN-us"].map(l => new Intl.Segmenter(l).resolvedOptions().locale).join() + JSON.stringify(new Intl.Segmenter("en", {granularity: "word"}).resolvedOptions())`, `en-US,zh-Hans,de-CH,en-US,en,en-US{"locale":"en","granularity":"word"}`},
		{"granularity", `return new Intl.Segmenter("en", {granularity: "line"})`, "!RangeError: Value line out of range for Intl.Segmenter options property granularity"},
		{"options null", `return new Intl.Segmenter("en", null)`, "!TypeError"},
		{"options string", `return new Intl.Segmenter("en", "word")`, "!TypeError"},
		{"bad locale", `return new Intl.Segmenter("en-")`, "!RangeError"},
		{"locale type", `return new Intl.Segmenter([1])`, "!TypeError"},
		{"localeMatcher", `return new Intl.Segmenter("en", {localeMatcher: "x"})`, "!RangeError: Value x out of range for Intl.Segmenter options property localeMatcher"},
		{"option order", `const log = []; const o = {get granularity() { log.push("granularity"); return "word" }, get localeMatcher() { log.push("localeMatcher"); return "lookup" }}; new Intl.Segmenter({get length() { log.push("length"); return 0 }}, o); return log.join()`, "length,localeMatcher,granularity"},
		{"supportedLocalesOf", `return Intl.Segmenter.supportedLocalesOf(["zxx", "en-US", "DE", "en-US"]).join() + Intl.Segmenter.supportedLocalesOf().length`, "en-US,de0"},
		{"supportedLocalesOf matcher", `return Intl.Segmenter.supportedLocalesOf("en", {localeMatcher: "fast"})`, "!RangeError"},
	})
}

func TestIntlLocale(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"maximize", `return [new Intl.Locale("zh-CN").maximize().script, new Intl.Locale("zh-CN").maximize().toString(), new Intl.Locale("en").maximize().toString(), new Intl.Locale("en-Latn-US").minimize().toString()].join()`, "Hans,zh-Hans-CN,en-Latn-US,en"},
		{"getters", `const l = new Intl.Locale("de-Latn-DE-1996-u-ca-gregory-co-phonebk-hc-h23-kf-upper-kn-nu-latn"); return [l.baseName, l.language, l.script, l.region, l.variants, l.calendar, l.collation, l.hourCycle, l.caseFirst, l.numeric, l.numberingSystem, l.firstDayOfWeek].join()`, "de-Latn-DE-1996,de,Latn,DE,1996,gregory,phonebk,h23,upper,true,latn,"},
		{"missing", `const l = new Intl.Locale("en"); return [l.script, l.region, l.variants, l.calendar, l.numeric].join()`, ",,,,false"},
		{"options", `return new Intl.Locale("en-US", {language: "fr", region: "CA", calendar: "islamicc", numeric: true, hourCycle: "h12", firstDayOfWeek: "1"}).toString()`, "fr-CA-u-ca-islamic-civil-fw-mon-hc-h12-kn"},
		{"locale argument", `const l = new Intl.Locale("ja-JP-u-ca-japanese"); return [new Intl.Locale(l).toString(), new Intl.Locale(l, {calendar: "gregory"}).calendar, Intl.getCanonicalLocales([l, "ja-jp-u-ca-japanese"]).length].join()`, "ja-JP-u-ca-japanese,gregory,1"},
		{"bad tag", `return new Intl.Locale("en-US-")`, "!RangeError"},
		{"tag type", `return new Intl.Locale(5)`, "!TypeError"},
		{"bad option", `return new Intl.Locale("en", {region: "USA"})`, "!RangeError"},
		{"hourCycle", `return new Intl.Locale("en", {hourCycle: "h13"})`, "!RangeError: Value h13 out of range for Intl.Locale options property hourCycle"},
		{"getCanonicalLocales", `return Intl.getCanonicalLocales(["EN-us", "zh-hans-cn", "en-US"]).join() + Intl.getCanonicalLocales().length`, "en-US,zh-Hans-CN0"},
		{"getCanonicalLocales errors", `try { Intl.getCanonicalLocales([NaN]) } catch (e) { return e.name + Intl.getCanonicalLocales.call(null, "de").join() }`, "TypeErrorde"},
	})
}

// TestIntlLateGroup checks that Intl costs a realm nothing until a program
// names it, and that the shared realms build it once per process.
func TestIntlLateGroup(t *testing.T) {
	bit := uint32(1) << lateIntl
	r1 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r2 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	m := NewRealm()
	for _, r := range []*Realm{r1, r2, m} {
		assert.NotZero(t, r.coldGlobals&bit, "Intl is pending at creation")
	}
	assert.Nil(t, m.intl)
	for _, r := range []*Realm{r1, r2, m} {
		v, err := r.Global.Get(r, StringKey(AtomIntl), ObjectValue(r.Global))
		require.NoError(t, err)
		require.True(t, v.IsObject())
		assert.Zero(t, r.coldGlobals&bit)
	}
	assert.Same(t, r1.intl.Intl, r2.intl.Intl)
	assert.True(t, r1.intl.SegmenterPrototype.IsShared())
	assert.NotSame(t, r1.intl.Intl, m.intl.Intl)
	assert.False(t, m.intl.SegmenterPrototype.IsShared())
}

// TestIntlSegmentAllocs checks what a step of a segment iterator allocates:
// the segment data object, and the segment's string unless it is one ASCII
// character.
func TestIntlSegmentAllocs(t *testing.T) {
	for _, c := range []struct {
		text string
		gran string
		want float64
	}{
		{"abcdefgh", "grapheme", 1},
		{"a b c d ", "word", 1},
		{"ab cd ef", "word", 2},
		{"中文字符中文字符", "grapheme", 2},
	} {
		t.Run(c.text+"/"+c.gran, func(t *testing.T) {
			m, err := syntax.ParseModule("t.js", `export function f(t, g) { return new Intl.Segmenter("en", {granularity: g}).segment(t)[Symbol.iterator]() }`, syntax.Options{})
			require.NoError(t, err)
			code, err := compiler.CompileModule(m)
			require.NoError(t, err)
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := r.EvaluateModule(code)
			require.NoError(t, err)
			fn, _ := env.GetBindingValue("f")
			it, err := r.Call(fn, Undefined(), []Value{StringValue(FromGoString(c.text)), StringValue(FromGoString(c.gran))})
			require.NoError(t, err)
			sg := it.AsObject().internal.(*intlSegments)
			n := testing.AllocsPerRun(100, func() {
				if _, done := sg.step(r); done {
					sg.pos = 0
				}
			})
			assert.LessOrEqual(t, n, c.want)
		})
	}
}

// TestIntlAtomNames checks that intlAtomNames lists the strings of
// intlAtoms' fields in field order: each field's string is its name but for
// the ones below.
func TestIntlAtomNames(t *testing.T) {
	other := map[string]string{
		"bestFit": "best fit", "localeName": "Locale", "localeTag": "Intl.Locale", "falseStr": "false",
		"segmenterName": "Segmenter", "segmenterTag": "Intl.Segmenter", "segmentIteratorTag": "Segmenter String Iterator",
	}
	a := reflect.ValueOf(intlNames()).Elem()
	require.Equal(t, len(intlAtomNames), a.NumField())
	for i := range a.NumField() {
		name := a.Type().Field(i).Name
		want, ok := other[name]
		if !ok {
			want = name
		}
		assert.Equal(t, want, (*String)(a.Field(i).UnsafePointer()).GoString(), name)
	}
}

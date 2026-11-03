package engine

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dateEval runs a module in a fresh realm with local time zone loc and
// returns ToString of its export r, or "throws Name: message".
func dateEval(t *testing.T, loc *time.Location, src string) string {
	t.Helper()
	m, err := syntax.ParseModule("date.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true, TimeZone: loc})
	env, err := r.EvaluateModule(code)
	if err != nil {
		var exc *Exception
		if errors.As(err, &exc) {
			return "throws " + errorDisplayString(exc.Value)
		}
		return "go: " + err.Error()
	}
	v, _ := env.GetBindingValue("r")
	s, err := r.ToString(v)
	require.NoError(t, err)
	return s.GoString()
}

func loadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s: %v", name, err)
	}
	return loc
}

// TestDateV8 runs testdata/date_probe.js in every time zone recorded in
// testdata/date_v8.json.gz and compares each result with V8's.
func TestDateV8(t *testing.T) {
	probe, err := os.ReadFile("testdata/date_probe.js")
	require.NoError(t, err)
	f, err := os.Open("testdata/date_v8.json.gz")
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var want struct {
		Node  string
		Zones map[string][][2]any
	}
	require.NoError(t, json.Unmarshal(raw, &want))
	require.NotEmpty(t, want.Zones)
	src := string(probe) + "\nexport const r = JSON.stringify(probe());\n"
	// The @@toPrimitive probes need the global Symbol.
	hasSymbol := dateEval(t, time.UTC, "export const r = typeof Symbol;") != "undefined"
	for zone, cases := range want.Zones {
		t.Run(zone, func(t *testing.T) {
			out := dateEval(t, loadZone(t, zone), src)
			var got [][2]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			require.Len(t, got, len(cases))
			bad := 0
			for i, c := range cases {
				key := c[0].(string)
				require.Equal(t, key, got[i][0])
				if !hasSymbol && strings.Contains(key, "toPrimitive") {
					continue
				}
				if !assert.Equal(t, c[1], got[i][1], "%s: %s", zone, key) {
					if bad++; bad > 40 {
						t.Fatal("too many mismatches")
					}
				}
			}
		})
	}
}

// TestDateToPrimitive calls Date.prototype[@@toPrimitive] directly.
func TestDateToPrimitive(t *testing.T) {
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	fn, err := r.DatePrototype.GetProp(r, SymbolKey(SymToPrimitive))
	require.NoError(t, err)
	require.True(t, fn.IsObject() && fn.AsObject().IsCallable())
	_, own := r.DatePrototype.GetOwnProperty(SymbolKey(SymToPrimitive))
	require.True(t, own)
	d, err := r.Construct(ObjectValue(r.DateCtor), []Value{IntValue(0)}, nil)
	require.NoError(t, err)
	call := func(this Value, hint Value) (Value, error) { return r.Call(fn, this, []Value{hint}) }
	v, err := call(d, str("number"))
	require.NoError(t, err)
	assert.Equal(t, 0.0, v.AsNumber())
	for _, h := range []string{"default", "string"} {
		v, err = call(d, str(h))
		require.NoError(t, err)
		assert.Equal(t, "Thu Jan 01 1970 00:00:00 GMT+0000 (UTC)", v.AsString().GoString())
	}
	_, err = call(d, str("Number"))
	assertErrorKind(t, err, KindTypeError, "Invalid hint")
	_, err = call(d, Undefined())
	assertErrorKind(t, err, KindTypeError, "Invalid hint")
	_, err = call(IntValue(1), str("number"))
	assertErrorKind(t, err, KindTypeError, "non-object")
	name, err := fn.AsObject().GetProp(r, StringKey(AtomName))
	require.NoError(t, err)
	assert.Equal(t, "[Symbol.toPrimitive]", name.AsString().GoString())
}

// TestDateZoneNames checks the parts TestDateV8 leaves out: the zone
// abbreviation from Go's tz database in toString and toTimeString.
func TestDateZoneNames(t *testing.T) {
	cases := []struct {
		zone string
		src  string
		want string
	}{
		{"UTC", "new Date(0).toString()", "Thu Jan 01 1970 00:00:00 GMT+0000 (UTC)"},
		{"America/New_York", "new Date(2024, 0, 2, 10, 5, 7, 9).toString()", "Tue Jan 02 2024 10:05:07 GMT-0500 (EST)"},
		{"America/New_York", "new Date(2024, 6, 2).toTimeString()", "00:00:00 GMT-0400 (EDT)"},
		{"America/New_York", "new Date(Date.UTC(1830, 0, 1)).toString()", "Thu Dec 31 1829 19:03:58 GMT-0456 (LMT)"},
		{"Asia/Shanghai", "new Date(2024, 0, 2).toString()", "Tue Jan 02 2024 00:00:00 GMT+0800 (CST)"},
		{"Australia/Lord_Howe", "new Date(2024, 6, 1).toString()", "Mon Jul 01 2024 00:00:00 GMT+1030 (+1030)"},
		{"Australia/Lord_Howe", "new Date(2024, 0, 1).toString()", "Mon Jan 01 2024 00:00:00 GMT+1100 (+11)"},
		{"Pacific/Apia", "new Date(2011, 11, 30, 12).toString()", "Sat Dec 31 2011 12:00:00 GMT+1400 (+14)"},
		{"Asia/Kolkata", "new Date(2024, 0, 2).toString()", "Tue Jan 02 2024 00:00:00 GMT+0530 (IST)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, dateEval(t, loadZone(t, c.zone), "export const r = "+c.src+";"), "%s: %s", c.zone, c.src)
	}
}

func TestDateMinimal(t *testing.T) {
	r := NewRealm()
	frozen := time.Date(2026, 9, 23, 12, 34, 56, 789_000_000, time.UTC)
	r.SetNow(func() time.Time { return frozen })
	r.SetTimeZone(time.UTC)
	now, err := callMethodErr(r, ObjectValue(r.DateCtor), "now")
	require.NoError(t, err)
	assert.Equal(t, Int64Value(frozen.UnixMilli()), now)
	d, err := r.Construct(ObjectValue(r.DateCtor), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Date", d.AsObject().ClassName())
	assert.Same(t, r.DatePrototype, d.AsObject().Proto())
	assert.Equal(t, NumberValue(float64(frozen.UnixMilli())), callMethod(t, r, d, "getTime"))
	assert.Equal(t, NumberValue(float64(frozen.UnixMilli())), callMethod(t, r, d, "valueOf"))
	assert.Equal(t, "2026-09-23T12:34:56.789Z", callMethod(t, r, d, "toISOString").AsString().GoString())
	assert.Equal(t, "2026-09-23T12:34:56.789Z", callMethod(t, r, d, "toJSON").AsString().GoString())
	tv, ok := d.AsObject().DateValue()
	assert.True(t, ok)
	assert.Equal(t, float64(frozen.UnixMilli()), tv)
	// Date() as a function returns the frozen time as toString does.
	s, err := r.Call(ObjectValue(r.DateCtor), Undefined(), []Value{IntValue(0)})
	require.NoError(t, err)
	assert.Equal(t, "Wed Sep 23 2026 12:34:56 GMT+0000 (UTC)", s.AsString().GoString())
	// Go entry points.
	g := r.NewDate(frozen)
	assert.Equal(t, "2026-09-23T12:34:56.789Z", callMethod(t, r, ObjectValue(g), "toISOString").AsString().GoString())
	assert.True(t, r.DateTime(tv).Equal(frozen))
	assert.Same(t, time.UTC, r.DateTime(tv).Location())
	// Primitive conversion: numbers through valueOf, strings through
	// @@toPrimitive's default hint.
	n, err := r.ToNumber(d)
	require.NoError(t, err)
	assert.Equal(t, float64(frozen.UnixMilli()), n)
	// The realm clock and zone are not shared with other realms, and the
	// defaults allocate nothing.
	assert.Nil(t, NewRealm().host)
	assert.Nil(t, NewRealmWith(RealmOptions{SharedIntrinsics: true}).host)
	assert.Same(t, time.Local, NewRealm().TimeZone())
	r.SetTimeZone(nil)
	assert.Same(t, time.Local, r.TimeZone())
	r.SetNow(nil)
	assert.WithinDuration(t, time.Now(), r.now(), time.Minute)
}

// TestDatePrototypeDeferred checks that a mutable realm defines the methods
// of Date.prototype on the first look at it, whichever way that happens, in
// the order of a shared realm.
func TestDatePrototypeDeferred(t *testing.T) {
	r := NewRealm()
	assert.NotZero(t, r.DatePrototype.flags&flagHasLazy)
	g := r.NewDate(time.UnixMilli(0))
	assert.Equal(t, "1970-01-01T00:00:00.000Z", callMethod(t, r, ObjectValue(g), "toISOString").AsString().GoString())
	assert.Zero(t, r.DatePrototype.flags&flagHasLazy)

	keys := "Object.getOwnPropertyNames(Date.prototype).join()"
	want := evalModuleWith(t, "export function f() { return "+keys+"; }", RealmOptions{SharedIntrinsics: true}).call("f")
	for src, res := range map[string]any{
		keys: want,
		"Object.preventExtensions(Date.prototype), [Object.isExtensible(Date.prototype), typeof new Date(0).getTime]": "false,function",
		"Object.freeze(Date.prototype), [Object.isFrozen(Date.prototype), typeof Date.prototype.toISOString]":         "true,function",
		"Date.prototype.x = 1, Object.getOwnPropertyNames(Date.prototype).slice(-2)":                                  "toLocaleTimeString,x",
		"Object.defineProperty(Date.prototype, 'y', {}), Object.keys(Date.prototype).length + ',' + Date.prototype.y": "0,undefined",
		"delete Date.prototype.getTime, typeof new Date(0).getTime":                                                   "undefined",
		"['getTime' in Date.prototype, Date.prototype.hasOwnProperty('toJSON')]":                                      "true,true",
		"JSON.stringify(new Date(0)) + String(new Date(NaN))":                                                         `"1970-01-01T00:00:00.000Z"Invalid Date`,
		"[1, 2, 3].map(n => new Date(n)).reduce((s, d) => s + d.getTime(), 0)":                                        "6",
	} {
		assert.Equal(t, res, evalModule(t, "export function f() { return String(("+src+")); }").call("f"), src)
	}
}

// TestDateProtoSlabFilled pins dateProtoSlab to exactly what the deferred
// Date.prototype install uses and to the 18,432-byte size class (with the
// 8-byte malloc header).
func TestDateProtoSlabFilled(t *testing.T) {
	r := NewRealm()
	p := r.DatePrototype
	p.internal, p.flags = nil, p.flags&^flagHasLazy
	b := &bootstrapSlabs{
		funcs:  make([]nativeFuncObject, 0, 100),
		shapes: make([]Shape, 0, 100),
		trans:  make([]transition, 0, 100),
		slots:  make([]Value, 0, 100),
	}
	r.boot = b
	installDatePrototype(r, p)
	r.boot = nil
	var s dateProtoSlab
	assert.Equal(t, len(s.funcs), len(b.funcs))
	assert.Equal(t, len(s.shapes), len(b.shapes))
	assert.Equal(t, len(s.trans), len(b.trans))
	assert.Equal(t, len(s.slots), len(b.slots))
	assert.LessOrEqual(t, unsafe.Sizeof(s)+8, uintptr(18432))
	assert.Equal(t, 1.0, testing.AllocsPerRun(10, func() { newDateProtoSlab() }))
}

func TestDateConstructorTable(t *testing.T) {
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	cases := []struct {
		name string
		arg  Value
		want float64 // time value; NaN for invalid
		iso  string
	}{
		{"ms", IntValue(0), 0, "1970-01-01T00:00:00.000Z"},
		{"ms fractional truncates", NumberValue(1.9), 1, "1970-01-01T00:00:00.001Z"},
		{"negative ms", IntValue(-1), -1, "1969-12-31T23:59:59.999Z"},
		{"numeric string", str("12"), 1007164800000, "2001-12-01T00:00:00.000Z"},
		{"NaN", NaN(), math.NaN(), ""},
		{"too large", NumberValue(8.64e15 + 1), math.NaN(), ""},
		{"max", NumberValue(8.64e15), 8.64e15, "+275760-09-13T00:00:00.000Z"},
		{"min", NumberValue(-8.64e15), -8.64e15, "-271821-04-20T00:00:00.000Z"},
		{"date only is UTC", str("2024-02-29"), 1709164800000, "2024-02-29T00:00:00.000Z"},
		{"year month", str("2024-02"), 1706745600000, "2024-02-01T00:00:00.000Z"},
		{"year", str("2024"), 1704067200000, "2024-01-01T00:00:00.000Z"},
		{"full Z", str("2024-09-23T10:20:30.400Z"), 1727086830400, "2024-09-23T10:20:30.400Z"},
		{"offset", str("2024-09-23T10:20:30+02:00"), 1727079630000, "2024-09-23T08:20:30.000Z"},
		{"negative offset", str("2024-09-23T10:20:30.5-01:30"), 1727092230500, "2024-09-23T11:50:30.500Z"},
		{"one fraction digit", str("2024-09-23T10:20:30.5Z"), 1727086830500, "2024-09-23T10:20:30.500Z"},
		{"long fraction truncates", str("2024-09-23T10:20:30.123456Z"), 1727086830123, "2024-09-23T10:20:30.123Z"},
		{"hour 24", str("2024-09-23T24:00:00Z"), 1727136000000, "2024-09-24T00:00:00.000Z"},
		{"expanded year", str("+010000-01-01T00:00:00Z"), 253402300800000, "+010000-01-01T00:00:00.000Z"},
		{"negative expanded year", str("-000001-01-01T00:00:00Z"), -62198755200000, "-000001-01-01T00:00:00.000Z"},
		{"invalid month", str("2024-13-01"), math.NaN(), ""},
		{"day overflows into the next month as in V8", str("2024-02-30"), 1709251200000, "2024-03-01T00:00:00.000Z"},
		{"invalid hour", str("2024-01-01T25:00Z"), math.NaN(), ""},
		{"hour 24 with minutes", str("2024-01-01T24:01Z"), math.NaN(), ""},
		{"missing minutes", str("2024-01-01T10Z"), math.NaN(), ""},
		{"trailing garbage", str("2024-01-01x"), math.NaN(), ""},
		{"minus zero year falls back to the legacy grammar", str("-000000-01-01"), 978307200000, "2001-01-01T00:00:00.000Z"},
		{"month name", str("Sep 23 2024"), 1727049600000, "2024-09-23T00:00:00.000Z"},
		{"rfc2822", str("Mon, 23 Sep 2024 10:20:30 GMT"), 1727086830000, "2024-09-23T10:20:30.000Z"},
		{"empty", str(""), math.NaN(), ""},
		{"fullwidth digits", str("２０２４"), math.NaN(), ""},
		{"boolean", True(), 1, "1970-01-01T00:00:00.001Z"},
		{"null", Null(), 0, "1970-01-01T00:00:00.000Z"},
		{"undefined", Undefined(), math.NaN(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := r.Construct(ObjectValue(r.DateCtor), []Value{c.arg}, nil)
			require.NoError(t, err)
			tv := callMethod(t, r, d, "getTime").AsNumber()
			if math.IsNaN(c.want) {
				assert.True(t, math.IsNaN(tv), "got %v", tv)
				_, err := callMethodErr(r, d, "toISOString")
				assertErrorKind(t, err, KindRangeError, "Invalid time value")
				assert.True(t, callMethod(t, r, d, "toJSON").IsNull())
				assert.Equal(t, "Invalid Date", callMethod(t, r, d, "toString").AsString().GoString())
				return
			}
			assert.Equal(t, c.want, tv)
			assert.Equal(t, c.iso, callMethod(t, r, d, "toISOString").AsString().GoString())
		})
	}
	// Local-time parsing follows the realm's zone.
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	r.SetTimeZone(ny)
	d, err := r.Construct(ObjectValue(r.DateCtor), []Value{str("2024-06-15T08:30:00")}, nil)
	require.NoError(t, err)
	want := time.Date(2024, 6, 15, 8, 30, 0, 0, ny).UnixMilli()
	assert.Equal(t, float64(want), callMethod(t, r, d, "getTime").AsNumber())
	assert.Same(t, ny, r.DateTime(float64(want)).Location())
	// A Date argument copies the time value without calling valueOf.
	d2, err := r.Construct(ObjectValue(r.DateCtor), []Value{d}, nil)
	require.NoError(t, err)
	assert.Equal(t, float64(want), callMethod(t, r, d2, "getTime").AsNumber())
	// Components are local time.
	d3, err := r.Construct(ObjectValue(r.DateCtor), []Value{IntValue(2024), IntValue(5), IntValue(15), IntValue(8), IntValue(30)}, nil)
	require.NoError(t, err)
	assert.Equal(t, float64(want), callMethod(t, r, d3, "getTime").AsNumber())
	// Method receivers must be Dates.
	_, err = callMethodErr(r, ObjectValue(r.DatePrototype), "getTime")
	assertErrorKind(t, err, KindTypeError, "requires that 'this' be a Date")
	getTime, _ := r.DatePrototype.GetProp(r, StringKey(AtomGetTime))
	_, err = r.Call(getTime, IntValue(1), nil)
	assertErrorKind(t, err, KindTypeError, "requires that 'this' be a Date")
	// Date.prototype has no [[DateValue]] and is a plain object.
	_, ok := r.DatePrototype.DateValue()
	assert.False(t, ok)
	// Subclass-style newTarget prototype.
	ctor := r.NewNativeConstructor(AtomEmpty, 0, nil, func(r *Realm, args []Value, nt *Object) (Value, error) { return Undefined(), nil })
	proto := r.NewObjectWithProto(r.DatePrototype)
	require.NoError(t, ctor.SetProp(r, StringKey(AtomPrototype), ObjectValue(proto)))
	sub, err := r.Construct(ObjectValue(r.DateCtor), []Value{IntValue(5)}, ctor)
	require.NoError(t, err)
	assert.Same(t, proto, sub.AsObject().Proto())
	assert.Equal(t, 5.0, callMethod(t, r, sub, "getTime").AsNumber())
}

// TestDateArithmetic checks the exact calendar arithmetic against Go's time
// package over the whole TimeClip range.
func TestDateArithmetic(t *testing.T) {
	for _, tv := range []int64{0, -1, 1, 951782400000, -62135596800000, -62135596800001,
		8.64e15, -8.64e15, 253402300799999, 253402300800000, -2208988800001} {
		f := splitTime(tv)
		g := time.UnixMilli(tv).UTC()
		assert.Equal(t, int64(g.Year()), f.year, "%d", tv)
		assert.Equal(t, int(g.Month())-1, f.month, "%d", tv)
		assert.Equal(t, g.Day(), f.day, "%d", tv)
		assert.Equal(t, int(g.Weekday()), f.weekday, "%d", tv)
		assert.Equal(t, g.Hour(), f.hour, "%d", tv)
		assert.Equal(t, g.Nanosecond()/1e6, f.ms, "%d", tv)
		assert.Equal(t, float64(tv), makeDate(makeDay(float64(f.year), float64(f.month), float64(f.day)),
			makeTime(float64(f.hour), float64(f.minute), float64(f.second), float64(f.ms))), "%d", tv)
	}
	for d := int64(-100_000_001); d <= 100_000_001; d += 99_991 {
		y, m, day := civilFromDays(d)
		require.Equal(t, d, daysFromCivil(y, m, day), "day %d", d)
	}
	assert.True(t, math.IsNaN(makeDay(1e14, 0, 1)))
	assert.Equal(t, -1.0, makeDay(1970, 0, 0))
	assert.Equal(t, 31.0, makeDay(1970, 1, 1))
	assert.Equal(t, 0.0, makeDay(1969, 12, 1))
	assert.Equal(t, 0.0, makeDay(1971, -12, 1))
	assert.Equal(t, 59.0, makeDay(1970, 0, 60))
	assert.True(t, math.IsNaN(makeTime(math.Inf(1), 0, 0, 0)))
	assert.Equal(t, 1900.0, makeFullYear(0))
	assert.Equal(t, 1999.0, makeFullYear(99.9))
	assert.Equal(t, 100.0, makeFullYear(100))
	assert.Equal(t, -1.0, makeFullYear(-1))
	assert.True(t, math.IsNaN(timeClip(8.64e15+1)))
	assert.Equal(t, 0.0, timeClip(-0.5))
	assert.False(t, math.Signbit(timeClip(math.Copysign(0, -1))))
}

// TestDateUTCFromLocal covers LocalTZA(t, false) at transitions: repeated
// local times take the first occurrence, skipped ones the offset before the
// transition.
func TestDateUTCFromLocal(t *testing.T) {
	cases := []struct {
		zone  string
		local time.Time // wall clock fields only
		want  string    // RFC 3339 instant
	}{
		{"America/New_York", time.Date(2024, 3, 10, 2, 30, 0, 0, time.UTC), "2024-03-10T07:30:00Z"},
		{"America/New_York", time.Date(2024, 11, 3, 1, 30, 0, 0, time.UTC), "2024-11-03T05:30:00Z"},
		{"America/New_York", time.Date(2024, 11, 3, 2, 0, 0, 0, time.UTC), "2024-11-03T07:00:00Z"},
		{"Australia/Lord_Howe", time.Date(2024, 4, 7, 1, 45, 0, 0, time.UTC), "2024-04-06T14:45:00Z"},
		{"Australia/Lord_Howe", time.Date(2024, 10, 6, 2, 15, 0, 0, time.UTC), "2024-10-05T15:45:00Z"},
		{"Pacific/Apia", time.Date(2011, 12, 30, 12, 0, 0, 0, time.UTC), "2011-12-30T22:00:00Z"},
		{"Asia/Shanghai", time.Date(1991, 4, 14, 2, 30, 0, 0, time.UTC), "1991-04-13T18:30:00Z"},
	}
	for _, c := range cases {
		r := NewRealmWith(RealmOptions{TimeZone: loadZone(t, c.zone)})
		got := time.UnixMilli(r.utcFromLocal(c.local.UnixMilli())).UTC().Format(time.RFC3339)
		assert.Equal(t, c.want, got, "%s %s", c.zone, c.local.Format("2006-01-02 15:04"))
	}
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	assert.True(t, math.IsNaN(r.utc(maxLocalTimeValue+1)))
	assert.True(t, math.IsNaN(r.utc(math.NaN())))
	assert.Equal(t, 8.64e15, r.utc(8.64e15))
}

// TestDateParseGrammar pins parser edge cases that do not depend on the
// zone, including inputs longer than any fixed buffer.
func TestDateParseGrammar(t *testing.T) {
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	cases := []struct {
		in   string
		want float64
	}{
		{"Jan 2 2024 10:00 +596523:00", -443293200000},
		{"Jan 2 2024 10:00 +1193047:00", 1704187696000},
		{"Jan 2 2024 10:00 +0099", 1704183660000},
		{"Jan 2 2024 GMT-8:30", 1704184200000},
		{"Tue, 02 Jan 2024 15:05:07 GMT", 1704207907000},
		{"Jan 2 2024 \x00" + strings.Repeat("x", 5000), 1704153600000},
		{"(" + strings.Repeat("(x", 5000) + ") Jan 2 2024", math.NaN()},
		{"(" + strings.Repeat("x", 5000) + ") Jan 2 2024", 1704153600000},
		{"Jan 2 " + strings.Repeat("0", 5000) + "2024", 1704153600000},
		{"Jan 2 2024 " + strings.Repeat(" ", 5000) + "10:00", 1704189600000},
	}
	for _, c := range cases {
		got := r.parseDateString(FromGoString(c.in))
		if math.IsNaN(c.want) {
			assert.True(t, math.IsNaN(got), "%.40q", c.in)
			continue
		}
		assert.Equal(t, c.want, got, "%.40q", c.in)
	}
}

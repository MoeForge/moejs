package engine

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Strings used across the tables: ascii, Chinese (BMP non-ASCII), emoji
// (surrogate pairs) and lone surrogates.
var (
	sAscii   = "hello world"
	sChinese = "你好，世界"
	sEmoji   = "a😀b😀"
)

func TestStringMethodTables(t *testing.T) {
	r := NewRealm()
	lone := utf16Str('x', 0xD800, 'y')
	num := func(f float64) Value { return NumberValue(f) }
	cases := []struct {
		name   string
		this   Value
		method string
		args   []Value
		want   any // string, float64, bool or nil for undefined
	}{
		// charAt / charCodeAt / codePointAt / at
		{"charAt ascii", str(sAscii), "charAt", []Value{IntValue(4)}, "o"},
		{"charAt default 0", str(sAscii), "charAt", nil, "h"},
		{"charAt negative", str(sAscii), "charAt", []Value{IntValue(-1)}, ""},
		{"charAt out of range", str(sAscii), "charAt", []Value{IntValue(100)}, ""},
		{"charAt NaN", str(sAscii), "charAt", []Value{NaN()}, "h"},
		{"charAt Infinity", str(sAscii), "charAt", []Value{num(math.Inf(1))}, ""},
		{"charAt fractional", str(sAscii), "charAt", []Value{num(1.9)}, "e"},
		{"charAt chinese", str(sChinese), "charAt", []Value{IntValue(1)}, "好"},
		{"charAt emoji high", str(sEmoji), "charAt", []Value{IntValue(1)}, "\ufffd"}, // lone surrogate exports as U+FFFD
		{"charCodeAt emoji", str(sEmoji), "charCodeAt", []Value{IntValue(1)}, float64(0xD83D)},
		{"charCodeAt emoji low", str(sEmoji), "charCodeAt", []Value{IntValue(2)}, float64(0xDE00)},
		{"charCodeAt out", str(sAscii), "charCodeAt", []Value{IntValue(11)}, math.NaN()},
		{"charCodeAt string index", str(sAscii), "charCodeAt", []Value{str("1")}, 101.0},
		{"codePointAt emoji", str(sEmoji), "codePointAt", []Value{IntValue(1)}, float64(0x1F600)},
		{"codePointAt low half", str(sEmoji), "codePointAt", []Value{IntValue(2)}, float64(0xDE00)},
		{"codePointAt lone", lone, "codePointAt", []Value{IntValue(1)}, float64(0xD800)},
		{"codePointAt out", str(sAscii), "codePointAt", []Value{IntValue(-1)}, nil},
		{"at positive", str(sAscii), "at", []Value{IntValue(0)}, "h"},
		{"at negative", str(sAscii), "at", []Value{IntValue(-1)}, "d"},
		{"at out", str(sAscii), "at", []Value{IntValue(-12)}, nil},
		{"at chinese negative", str(sChinese), "at", []Value{IntValue(-1)}, "界"},
		// indexOf / lastIndexOf / includes / startsWith / endsWith
		{"indexOf", str(sAscii), "indexOf", []Value{str("o")}, 4.0},
		{"indexOf from", str(sAscii), "indexOf", []Value{str("o"), IntValue(5)}, 7.0},
		{"indexOf negative from", str(sAscii), "indexOf", []Value{str("h"), IntValue(-5)}, 0.0},
		{"indexOf beyond", str(sAscii), "indexOf", []Value{str("h"), IntValue(50)}, -1.0},
		{"indexOf empty beyond", str(sAscii), "indexOf", []Value{str(""), IntValue(50)}, 11.0},
		{"indexOf missing", str(sAscii), "indexOf", []Value{str("z")}, -1.0},
		{"indexOf undefined", str("undefined"), "indexOf", nil, 0.0},
		{"indexOf chinese", str(sChinese), "indexOf", []Value{str("世界")}, 3.0},
		{"indexOf emoji", str(sEmoji), "indexOf", []Value{str("😀"), IntValue(2)}, 4.0},
		{"lastIndexOf", str(sAscii), "lastIndexOf", []Value{str("o")}, 7.0},
		{"lastIndexOf from", str(sAscii), "lastIndexOf", []Value{str("o"), IntValue(6)}, 4.0},
		{"lastIndexOf NaN from", str(sAscii), "lastIndexOf", []Value{str("o"), NaN()}, 7.0},
		{"lastIndexOf negative", str(sAscii), "lastIndexOf", []Value{str("o"), IntValue(-1)}, -1.0},
		{"lastIndexOf zero start", str(sAscii), "lastIndexOf", []Value{str("h"), IntValue(-1)}, 0.0},
		{"lastIndexOf empty", str(sAscii), "lastIndexOf", []Value{str("")}, 11.0},
		{"lastIndexOf chinese", str(sChinese), "lastIndexOf", []Value{str("好")}, 1.0},
		{"includes", str(sAscii), "includes", []Value{str("lo w")}, true},
		{"includes pos", str(sAscii), "includes", []Value{str("h"), IntValue(1)}, false},
		{"includes empty", str(sAscii), "includes", []Value{str("")}, true},
		{"includes emoji", str(sEmoji), "includes", []Value{str("😀b")}, true},
		{"startsWith", str(sAscii), "startsWith", []Value{str("hell")}, true},
		{"startsWith pos", str(sAscii), "startsWith", []Value{str("world"), IntValue(6)}, true},
		{"startsWith negative pos", str(sAscii), "startsWith", []Value{str("hello"), IntValue(-3)}, true},
		{"startsWith too long", str("hi"), "startsWith", []Value{str("hi there")}, false},
		{"startsWith chinese", str(sChinese), "startsWith", []Value{str("你好")}, true},
		{"endsWith", str(sAscii), "endsWith", []Value{str("world")}, true},
		{"endsWith pos", str(sAscii), "endsWith", []Value{str("hello"), IntValue(5)}, true},
		{"endsWith Infinity", str(sAscii), "endsWith", []Value{str("d"), num(math.Inf(1))}, true},
		{"endsWith too long", str("hi"), "endsWith", []Value{str("ohi")}, false},
		{"endsWith emoji", str(sEmoji), "endsWith", []Value{str("😀")}, true},
		{"endsWith emoji low only", str(sEmoji), "endsWith", []Value{utf16Str(0xDE00)}, true},
		// slice / substring / substr
		{"slice", str(sAscii), "slice", []Value{IntValue(6)}, "world"},
		{"slice negative", str(sAscii), "slice", []Value{IntValue(-5)}, "world"},
		{"slice range", str(sAscii), "slice", []Value{IntValue(1), IntValue(3)}, "el"},
		{"slice negative end", str(sAscii), "slice", []Value{IntValue(0), IntValue(-6)}, "hello"},
		{"slice reversed", str(sAscii), "slice", []Value{IntValue(5), IntValue(2)}, ""},
		{"slice NaN", str(sAscii), "slice", []Value{NaN(), IntValue(2)}, "he"},
		{"slice -Infinity", str(sAscii), "slice", []Value{num(math.Inf(-1)), IntValue(2)}, "he"},
		{"slice Infinity end", str(sAscii), "slice", []Value{IntValue(9), num(math.Inf(1))}, "ld"},
		{"slice chinese", str(sChinese), "slice", []Value{IntValue(3)}, "世界"},
		{"slice emoji split pair", str(sEmoji), "slice", []Value{IntValue(1), IntValue(2)}, "\ufffd"},
		{"substring", str(sAscii), "substring", []Value{IntValue(6)}, "world"},
		{"substring swapped", str(sAscii), "substring", []Value{IntValue(5), IntValue(0)}, "hello"},
		{"substring negative", str(sAscii), "substring", []Value{IntValue(-3), IntValue(2)}, "he"},
		{"substring NaN end", str(sAscii), "substring", []Value{IntValue(2), NaN()}, "he"},
		{"substring huge", str(sAscii), "substring", []Value{IntValue(2), num(1e300)}, "llo world"},
		{"substr", str(sAscii), "substr", []Value{IntValue(6), IntValue(3)}, "wor"},
		{"substr negative", str(sAscii), "substr", []Value{IntValue(-5), IntValue(2)}, "wo"},
		{"substr no length", str(sAscii), "substr", []Value{IntValue(6)}, "world"},
		{"substr zero length", str(sAscii), "substr", []Value{IntValue(1), IntValue(0)}, ""},
		{"substr negative length", str(sAscii), "substr", []Value{IntValue(1), IntValue(-1)}, ""},
		{"substr beyond", str(sAscii), "substr", []Value{IntValue(20), IntValue(5)}, ""},
		{"substr chinese", str(sChinese), "substr", []Value{IntValue(1), IntValue(2)}, "好，"},
		// case
		{"toLowerCase ascii", str("Hello WORLD"), "toLowerCase", nil, "hello world"},
		{"toUpperCase ascii", str("Hello world"), "toUpperCase", nil, "HELLO WORLD"},
		{"toLowerCase unicode", str("ÀÉÎ Straße ΣΑΣ"), "toLowerCase", nil, "àéî straße σας"},
		{"toLowerCase sigma alone", str("Σ"), "toLowerCase", nil, "σ"},
		{"toLowerCase dotted I", str("İ"), "toLowerCase", nil, "i̇"},
		{"toUpperCase sharp s", str("straße"), "toUpperCase", nil, "STRASSE"},
		{"toUpperCase ligature", str("ﬁsh"), "toUpperCase", nil, "FISH"},
		{"toUpperCase greek", str("ᾳ"), "toUpperCase", nil, "ΑΙ"},
		{"toUpperCase emoji", str(sEmoji), "toUpperCase", nil, "A😀B😀"},
		{"toUpperCase chinese", str(sChinese), "toUpperCase", nil, sChinese},
		{"toLowerCase turkish stays simple", str("I"), "toLowerCase", nil, "i"},
		// trim
		{"trim", str("  \t\n hi \u00a0\ufeff"), "trim", nil, "hi"},
		{"trimStart", str("\u2028 hi  "), "trimStart", nil, "hi  "},
		{"trimEnd", str("  hi\u3000"), "trimEnd", nil, "  hi"},
		{"trim all whitespace", str(" \u200a "), "trim", nil, ""},
		{"trim keeps zwsp", str("\u200bhi"), "trim", nil, "\u200bhi"},
		{"trim chinese", str(" 你好 "), "trim", nil, "你好"},
		// concat / repeat / pad
		{"concat", str("a"), "concat", []Value{str("b"), IntValue(1), Null()}, "ab1null"},
		{"concat none", str("a"), "concat", nil, "a"},
		{"repeat", str("ab"), "repeat", []Value{IntValue(3)}, "ababab"},
		{"repeat zero", str("ab"), "repeat", []Value{IntValue(0)}, ""},
		{"repeat NaN", str("ab"), "repeat", []Value{NaN()}, ""},
		{"repeat fractional", str("ab"), "repeat", []Value{num(2.7)}, "abab"},
		{"repeat unicode", str("好"), "repeat", []Value{IntValue(3)}, "好好好"},
		{"repeat empty huge", str(""), "repeat", []Value{IntValue(1 << 20)}, ""},
		{"padStart", str("5"), "padStart", []Value{IntValue(3), str("0")}, "005"},
		{"padStart default", str("5"), "padStart", []Value{IntValue(3)}, "  5"},
		{"padStart truncate filler", str("abc"), "padStart", []Value{IntValue(8), str("123456")}, "12345abc"},
		{"padStart shorter", str("abc"), "padStart", []Value{IntValue(2)}, "abc"},
		{"padStart empty filler", str("abc"), "padStart", []Value{IntValue(10), str("")}, "abc"},
		{"padEnd", str("abc"), "padEnd", []Value{IntValue(6), str("xy")}, "abcxyx"},
		{"padEnd unicode", str("你"), "padEnd", []Value{IntValue(3), str("好")}, "你好好"},
		// localeCompare / toString / valueOf
		{"localeCompare less", str("a"), "localeCompare", []Value{str("b")}, -1.0},
		{"localeCompare equal", str("a"), "localeCompare", []Value{str("a")}, 0.0},
		{"localeCompare greater", str("b"), "localeCompare", []Value{str("a")}, 1.0},
		{"localeCompare unit order", str("😀"), "localeCompare", []Value{str("\uffff")}, -1.0},
		{"toString", str("x"), "toString", nil, "x"},
		{"valueOf", str("x"), "valueOf", nil, "x"},
		// this coercion (String.prototype methods applied to other primitives)
		{"number this", IntValue(123), "charAt", []Value{IntValue(1)}, "2"},
		{"bool this", True(), "toUpperCase", nil, "TRUE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn, err := r.StringPrototype.GetProp(r, r.KeyFromGoString(c.method))
			require.NoError(t, err)
			got, err := r.Call(fn, c.this, c.args)
			require.NoError(t, err)
			switch want := c.want.(type) {
			case string:
				require.True(t, got.IsString(), "got %v", got)
				assert.Equal(t, want, got.AsString().GoString())
			case float64:
				require.True(t, got.IsNumber(), "got %v", got)
				if math.IsNaN(want) {
					assert.True(t, math.IsNaN(got.AsNumber()))
				} else {
					assert.Equal(t, want, got.AsNumber())
				}
			case bool:
				assert.Equal(t, Bool(want), got)
			case nil:
				assert.True(t, got.IsUndefined(), "got %v", got)
			}
		})
	}
}

func TestStringSubstringsAreZeroCopy(t *testing.T) {
	r := NewRealm()
	src := strings.Repeat("abcdefghij", 10) + "  "
	s := FromGoString(src)
	for _, c := range []struct {
		method string
		args   []Value
		want   string
	}{
		{"slice", []Value{IntValue(10), IntValue(20)}, "abcdefghij"},
		{"substring", []Value{IntValue(90)}, "abcdefghij  "},
		{"substr", []Value{IntValue(0), IntValue(5)}, "abcde"},
		{"trim", nil, src[:100]},
		{"trimEnd", nil, src[:100]},
	} {
		got := callMethod(t, r, StringValue(s), c.method, c.args...)
		require.Equal(t, c.want, got.AsString().GoString())
		a, ok := got.AsString().ASCII()
		require.True(t, ok)
		// Same backing memory: the substring's data pointer lies inside src.
		assert.True(t, strings.HasPrefix(src[strings.Index(src, a):], a))
		assert.Equal(t, strASCII, got.AsString().kind)
	}
	// Nothing to trim returns the very same *String.
	trimmed := callMethod(t, r, StringValue(FromGoString("clean")), "trim")
	assert.Equal(t, "clean", trimmed.AsString().GoString())
	lower := callMethod(t, r, str("already lower 123"), "toLowerCase")
	assert.Equal(t, "already lower 123", lower.AsString().GoString())
}

func TestStringMethodsOnRopes(t *testing.T) {
	r := NewRealm()
	rope := StringValue(concat(FromGoString(strings.Repeat("a", 40)), FromGoString(strings.Repeat("b", 40))))
	assert.Equal(t, 40.0, callMethod(t, r, rope, "indexOf", str("b")).AsNumber())
	assert.Equal(t, "ab", callMethod(t, r, rope, "slice", IntValue(39), IntValue(41)).AsString().GoString())
	assert.True(t, callMethod(t, r, rope, "endsWith", str("bb")).AsBool())
	mixed := StringValue(concat(FromGoString(strings.Repeat("a", 40)), FromGoString("你好世界你好世界你好世界你好世界你好世界你好世界")))
	assert.Equal(t, 40.0, callMethod(t, r, mixed, "indexOf", str("你")).AsNumber())
	assert.Equal(t, "A", callMethod(t, r, mixed, "toUpperCase").AsString().Substring(0, 1).GoString())
}

func TestStringErrors(t *testing.T) {
	r := NewRealm()
	_, err := callMethodErr(r, str("a"), "repeat", IntValue(-1))
	assertErrorKind(t, err, KindRangeError, "Invalid count value")
	_, err = callMethodErr(r, str("a"), "repeat", NumberValue(math.Inf(1)))
	assertErrorKind(t, err, KindRangeError, "Invalid count value")
	_, err = callMethodErr(r, str("ab"), "repeat", IntValue(1<<30))
	assertErrorKind(t, err, KindRangeError, "Invalid string length")
	_, err = callMethodErr(r, str("a"), "normalize", str("nfc"))
	assertErrorKind(t, err, KindRangeError, "The normalization form should be one of NFC, NFD, NFKC, NFKD.")
	trim, _ := r.StringPrototype.GetProp(r, StringKey(AtomTrim))
	_, err = r.Call(trim, Undefined(), nil)
	assertErrorKind(t, err, KindTypeError, "String.prototype.trim called on null or undefined")
	_, err = r.Call(trim, Null(), nil)
	assertErrorKind(t, err, KindTypeError, "called on null or undefined")
	rx := newRegExp(t, r, "a", "")
	for _, m := range []string{"includes", "startsWith", "endsWith"} {
		_, err = callMethodErr(r, str("a"), m, rx)
		assertErrorKind(t, err, KindTypeError, "must not be a regular expression")
	}
	// Coercion order: `this` is coerced before the argument.
	thrower := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.TypeError("boom")
	})
	bad := r.NewObject()
	require.NoError(t, bad.SetProp(r, StringKey(AtomToString), ObjectValue(thrower)))
	_, err = callMethodErr(r, str("a"), "indexOf", ObjectValue(bad))
	assertErrorKind(t, err, KindTypeError, "boom")
}

func TestStringFromCharCodeAndWrapper(t *testing.T) {
	r := NewRealm()
	v, err := r.Call(ObjectValue(mustGet(t, r, r.StringCtor, "fromCharCode")), Undefined(), []Value{IntValue(72), IntValue(105), IntValue(0x1F600 + 0x10000)})
	require.NoError(t, err)
	assert.Equal(t, "Hi", v.AsString().Substring(0, 2).GoString())
	assert.Equal(t, uint16(0xF600), v.AsString().At(2), "ToUint16 truncation")
	v, err = r.Call(ObjectValue(mustGet(t, r, r.StringCtor, "fromCharCode")), Undefined(), []Value{IntValue(0xD83D), IntValue(0xDE00)})
	require.NoError(t, err)
	assert.Equal(t, "😀", v.AsString().GoString())
	v, err = r.Call(ObjectValue(mustGet(t, r, r.StringCtor, "fromCharCode")), Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", v.AsString().GoString())
	// Wrapper objects work as `this`.
	w := r.NewStringObject(FromGoString("  wrapped "))
	assert.Equal(t, "wrapped", callMethod(t, r, ObjectValue(w), "trim").AsString().GoString())
	assert.Equal(t, 10, callMethod(t, r, ObjectValue(w), "valueOf").AsString().Len())
	lv, _ := w.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(10), lv)
	sv, err := r.Call(ObjectValue(r.StringCtor), Undefined(), []Value{IntValue(42)})
	require.NoError(t, err)
	assert.Equal(t, "42", sv.AsString().GoString())
}

func mustGet(t *testing.T, r *Realm, o *Object, name string) *Object {
	t.Helper()
	v, err := o.GetProp(r, r.KeyFromGoString(name))
	require.NoError(t, err)
	require.True(t, v.IsObject(), "%s missing", name)
	return v.AsObject()
}

func TestStringSplitTable(t *testing.T) {
	r := NewRealm()
	rx := func(p, f string) Value { return newRegExp(t, r, p, f) }
	cases := []struct {
		name string
		s    string
		sep  Value
		lim  Value
		want []any
	}{
		{"comma", "a,b,,c", str(","), Undefined(), []any{"a", "b", "", "c"}},
		{"limit", "a,b,c", str(","), IntValue(2), []any{"a", "b"}},
		{"limit zero", "a,b,c", str(","), IntValue(0), []any{}},
		{"limit negative is uint32", "a,b", str(","), IntValue(-1), []any{"a", "b"}},
		{"empty separator", "abc", str(""), Undefined(), []any{"a", "b", "c"}},
		{"empty separator limit", "abc", str(""), IntValue(2), []any{"a", "b"}},
		{"empty separator emoji units", "a😀", str(""), Undefined(), []any{"a", "\ufffd", "\ufffd"}},
		{"undefined separator", "a,b", Undefined(), Undefined(), []any{"a,b"}},
		{"undefined separator limit 0", "a,b", Undefined(), IntValue(0), []any{}},
		{"empty string", "", str(","), Undefined(), []any{""}},
		{"empty string empty sep", "", str(""), Undefined(), []any{}},
		{"sep not found", "abc", str("x"), Undefined(), []any{"abc"}},
		{"sep at ends", ",a,", str(","), Undefined(), []any{"", "a", ""}},
		{"multi-char sep", "a--b--c", str("--"), Undefined(), []any{"a", "b", "c"}},
		{"whole string sep", "abc", str("abc"), Undefined(), []any{"", ""}},
		{"chinese sep", "你，好，世界", str("，"), Undefined(), []any{"你", "好", "世界"}},
		{"chinese in ascii", "abc", str("，"), Undefined(), []any{"abc"}},
		{"number sep", "1a2a3", IntValue(2), Undefined(), []any{"1a", "a3"}},
		{"regexp", "a1b22c", rx(`\d+`, ""), Undefined(), []any{"a", "b", "c"}},
		{"regexp captures", "a1b2c", rx(`(\d)`, ""), Undefined(), []any{"a", "1", "b", "2", "c"}},
		{"regexp optional capture", "a1b", rx(`(x)?(\d)`, ""), Undefined(), []any{"a", nil, "1", "b"}},
		{"regexp captures limit", "a1b2c", rx(`(\d)`, ""), IntValue(2), []any{"a", "1"}},
		{"regexp empty match", "abc", rx("", ""), Undefined(), []any{"a", "b", "c"}},
		{"regexp empty match unicode", "a😀", rx("", "u"), Undefined(), []any{"a", "😀"}},
		{"regexp empty match non-unicode", "a😀", rx("", ""), Undefined(), []any{"a", "\ufffd", "\ufffd"}},
		{"regexp empty string match", "", rx("x*", ""), Undefined(), []any{}},
		{"regexp empty string no match", "", rx("x", ""), Undefined(), []any{""}},
		{"regexp star", "aXbXc", rx("X*", ""), Undefined(), []any{"a", "b", "c"}},
		{"regexp leading match", "Xa", rx("X", ""), Undefined(), []any{"", "a"}},
		{"regexp alternation", "one two  three", rx(`\s+`, ""), Undefined(), []any{"one", "two", "three"}},
		{"regexp ignores lastIndex", "a1b", rx(`\d`, "g"), Undefined(), []any{"a", "b"}},
		{"regexp sticky ignored", "a1b1c", rx(`1`, "y"), Undefined(), []any{"a", "b", "c"}},
		{"regexp anchored", "ab", rx(`^`, ""), Undefined(), []any{"ab"}},
		{"regexp unicode subject", "你1好2", rx(`\d`, ""), Undefined(), []any{"你", "好", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := callMethod(t, r, str(c.s), "split", c.sep, c.lim)
			assert.Equal(t, c.want, jsArrayB(r, got))
		})
	}
	// Splitting a global regexp does not touch lastIndex.
	g := newRegExp(t, r, `\d`, "g")
	require.NoError(t, g.AsObject().SetProp(r, StringKey(AtomLastIndex), IntValue(3)))
	callMethod(t, r, str("a1b"), "split", g)
	assert.Equal(t, "3", propString(t, r, g, "lastIndex"))
}

func TestStringReplaceTable(t *testing.T) {
	r := NewRealm()
	rx := func(p, f string) Value { return newRegExp(t, r, p, f) }
	cases := []struct {
		name    string
		s       string
		pattern Value
		repl    Value
		want    string
	}{
		{"string first only", "aaa", str("a"), str("b"), "baa"},
		{"string no match", "abc", str("x"), str("y"), "abc"},
		{"string empty pattern", "abc", str(""), str("-"), "-abc"},
		{"string $&", "hello world", str("world"), str("[$&]"), "hello [world]"},
		{"string $`", "hello world", str("world"), str("$`"), "hello hello "},
		{"string $'", "hello world", str("hello"), str("$'"), " world world"},
		{"string $$", "a", str("a"), str("$$"), "$"},
		{"string $ at end", "a", str("a"), str("x$"), "x$"},
		{"string $1 no groups is literal", "a", str("a"), str("$1"), "$1"},
		{"string $<name> no groups", "a", str("a"), str("$<x>"), "$<x>"},
		{"string unicode", "你好世界", str("世界"), str("moejs"), "你好moejs"},
		{"string emoji", sEmoji, str("😀"), str("!"), "a!b😀"},
		{"regexp $1 $2", "2024-09-23", rx(`(\d+)-(\d+)-(\d+)`, ""), str("$3/$2/$1"), "23/09/2024"},
		{"regexp $01", "abc", rx(`(b)`, ""), str("[$01]"), "a[b]c"},
		{"regexp $10 with one group is $1 + 0", "abc", rx(`(b)`, ""), str("$10"), "ab0c"},
		{"regexp $12 with 12 groups", "abcdefghijkl", rx(`(a)(b)(c)(d)(e)(f)(g)(h)(i)(j)(k)(l)`, ""), str("$12$1"), "la"},
		{"regexp $0 literal", "abc", rx(`b`, ""), str("$0"), "a$0c"},
		{"regexp $9 out of range literal", "abc", rx(`(b)`, ""), str("$9"), "a$9c"},
		{"regexp unmatched group is empty", "ac", rx(`a(b)?c`, ""), str("[$1]"), "[]"},
		{"regexp named", "John Smith", rx(`(?<first>\w+)\s(?<last>\w+)`, ""), str("$<last>, $<first>"), "Smith, John"},
		{"regexp named missing group empty", "ab", rx(`(?<x>a)`, ""), str("[$<y>]"), "[]b"},
		{"regexp named unterminated", "ab", rx(`(?<x>a)`, ""), str("$<x"), "$<xb"},
		{"regexp global", "a-b-c", rx(`-`, "g"), str("+"), "a+b+c"},
		{"regexp global whitespace", "a  b \t c", rx(`\s+`, "g"), str(" "), "a b c"},
		{"regexp global template", "x1y22", rx(`(\d+)`, "g"), str("<$1>"), "x<1>y<22>"},
		{"regexp global $` fallback", "abc", rx(`b`, "g"), str("$`"), "aac"},
		{"regexp global empty match", "abc", rx(``, "g"), str("-"), "-a-b-c-"},
		{"regexp global empty match star", "aaa", rx(`a*`, "g"), str("-"), "--"},
		{"regexp global empty unicode", "a😀", rx(``, "gu"), str("-"), "-a-😀-"},
		{"regexp global empty non-unicode", "😀", rx(``, "g"), str("-"), "-\ufffd-\ufffd-"},
		{"regexp non-global", "aaa", rx(`a`, ""), str("b"), "baa"},
		{"regexp case insensitive", "Hello HELLO", rx(`hello`, "gi"), str("x"), "x x"},
		{"regexp dot excludes newline", "a\nb", rx(`.`, "g"), str("x"), "x\nx"},
		{"regexp dotAll", "a\nb", rx(`.`, "gs"), str("x"), "xxx"},
		{"regexp multiline", "a\nb", rx(`^`, "gm"), str(">"), ">a\n>b"},
		{"regexp unicode subject", "你好 世界", rx(`\s`, "g"), str(""), "你好世界"},
		{"regexp unicode class", "café", rx(`[é]`, "g"), str("e"), "cafe"},
		{"regexp escaped dollar", "1.50", rx(`\.`, ""), str("$$"), "1$50"},
		{"regexp emoji dot u", "😀", rx(`^.$`, "u"), str("x"), "x"},
		{"regexp emoji dot no u", "😀", rx(`^..$`, ""), str("x"), "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := callMethod(t, r, str(c.s), "replace", c.pattern, c.repl)
			assert.Equal(t, c.want, got.AsString().GoString())
		})
	}
}

func TestStringReplaceAll(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		s, search, repl, want string
	}{
		{"aaa", "a", "b", "bbb"},
		{"abcabc", "bc", "-", "a-a-"},
		{"abc", "", "-", "-a-b-c-"},
		{"", "", "-", "-"},
		{"aXbXc", "X", "$&$&", "aXXbXXc"},
		{"你好你好", "你", "我", "我好我好"},
		{"a😀b😀", "😀", ".", "a.b."},
		{"no match", "z", "y", "no match"},
	}
	for _, c := range cases {
		got := callMethod(t, r, str(c.s), "replaceAll", str(c.search), str(c.repl))
		assert.Equal(t, c.want, got.AsString().GoString(), "%q.replaceAll(%q, %q)", c.s, c.search, c.repl)
	}
	got := callMethod(t, r, str("a1b2"), "replaceAll", newRegExp(t, r, `\d`, "g"), str("#"))
	assert.Equal(t, "a#b#", got.AsString().GoString())
	_, err := callMethodErr(r, str("a1b2"), "replaceAll", newRegExp(t, r, `\d`, ""), str("#"))
	assertErrorKind(t, err, KindTypeError, "replaceAll must be called with a global RegExp")
}

func TestStringReplaceFunctionReplacer(t *testing.T) {
	r := NewRealm()
	var calls [][]any
	fn := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		call := make([]any, len(args))
		for i, a := range args {
			call[i] = r.ToGo(a)
		}
		calls = append(calls, call)
		return str("<" + args[0].AsString().GoString() + ">"), nil
	}))
	got := callMethod(t, r, str("a-b"), "replace", str("-"), fn)
	assert.Equal(t, "a<->b", got.AsString().GoString())
	assert.Equal(t, [][]any{{"-", int64(1), "a-b"}}, calls)

	calls = nil
	got = callMethod(t, r, str("x1y22"), "replace", newRegExp(t, r, `(\d)(\d)?`, "g"), fn)
	assert.Equal(t, "x<1>y<22>", got.AsString().GoString())
	assert.Equal(t, [][]any{{"1", "1", nil, int64(1), "x1y22"}, {"22", "2", "2", int64(3), "x1y22"}}, calls)

	calls = nil
	got = callMethod(t, r, str("ab"), "replace", newRegExp(t, r, `(?<first>a)(?<second>b)`, ""), fn)
	assert.Equal(t, "<ab>", got.AsString().GoString())
	require.Len(t, calls, 1)
	require.Len(t, calls[0], 6)
	assert.Equal(t, map[string]any{"first": "a", "second": "b"}, calls[0][5])

	calls = nil
	got = callMethod(t, r, str("aXbXc"), "replaceAll", str("X"), fn)
	assert.Equal(t, "a<X>b<X>c", got.AsString().GoString())
	assert.Equal(t, [][]any{{"X", int64(1), "aXbXc"}, {"X", int64(3), "aXbXc"}}, calls)

	// The replacer's result is coerced with ToString.
	numFn := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) { return IntValue(7), nil }))
	got = callMethod(t, r, str("a-b"), "replace", str("-"), numFn)
	assert.Equal(t, "a7b", got.AsString().GoString())
	// Errors propagate.
	thrower := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.TypeError("replacer failed")
	}))
	_, err := callMethodErr(r, str("a-b"), "replace", str("-"), thrower)
	assertErrorKind(t, err, KindTypeError, "replacer failed")
}

func TestStringMatch(t *testing.T) {
	r := NewRealm()
	got := callMethod(t, r, str("a1b22c"), "match", newRegExp(t, r, `\d+`, "g"))
	assert.Equal(t, []any{"1", "22"}, jsArrayB(r, got))
	got = callMethod(t, r, str("abc"), "match", newRegExp(t, r, `\d+`, "g"))
	assert.True(t, got.IsNull())
	got = callMethod(t, r, str("a1b22c"), "match", newRegExp(t, r, `(\d)(\d)?`, ""))
	assert.Equal(t, []any{"1", "1", nil}, jsArrayB(r, got))
	assert.Equal(t, "1", propString(t, r, got, "index"))
	assert.Equal(t, "a1b22c", propString(t, r, got, "input"))
	assert.Equal(t, "undefined", propString(t, r, got, "groups"))
	// A string pattern is compiled as a regexp.
	got = callMethod(t, r, str("a.b"), "match", str("."))
	assert.Equal(t, []any{"a"}, jsArrayB(r, got))
	got = callMethod(t, r, str("abc"), "match")
	assert.Equal(t, []any{""}, jsArrayB(r, got))
	// Global match resets lastIndex and matches empties per spec.
	g := newRegExp(t, r, ``, "gu")
	got = callMethod(t, r, str("a😀"), "match", g)
	assert.Equal(t, []any{"", "", ""}, jsArrayB(r, got))
	assert.Equal(t, "0", propString(t, r, g, "lastIndex"))
}

func TestStringRepeatInterrupt(t *testing.T) {
	r := NewRealm()
	r.Interrupt("stop")
	_, err := callMethodErr(r, str("abcdefghij"), "repeat", IntValue(100000))
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	r.ClearInterrupt()
	v := callMethod(t, r, str("abcdefghij"), "repeat", IntValue(100000))
	assert.Equal(t, 1000000, v.AsString().Len())
}

// TestCaseSingleChar: a one-character ASCII string that changes case
// converts to the shared one-character string, without allocating.
func TestCaseSingleChar(t *testing.T) {
	r := NewRealm()
	for c := byte(0); c < 0x80; c++ {
		s := asciiString(string(rune(c)))
		up, err := stringToUpper(r, s)
		require.NoError(t, err)
		lo, err := stringToLower(r, s)
		require.NoError(t, err)
		assert.Equal(t, strings.ToUpper(string(rune(c))), up.GoString())
		assert.Equal(t, strings.ToLower(string(rune(c))), lo.GoString())
		assert.Equal(t, 0.0, testing.AllocsPerRun(10, func() { _, _ = stringToUpper(r, s); _, _ = stringToLower(r, s) }), "%q", c)
	}
}

// TestSplitEmptySeparatorLimit checks split("") with the default limit,
// 2**32 - 1, which is past an int on 32-bit systems, and with given ones.
func TestSplitEmptySeparatorLimit(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"no limit", []string{`JSON.stringify("abc".split(""))`}, `["a","b","c"]`},
		{"limit 2**32 - 1", []string{`"abc".split("", 2**32 - 1).length`}, "3"},
		{"limit 2", []string{`JSON.stringify("abc".split("", 2))`}, `["a","b"]`},
		{"limit -1", []string{`"abc".split("", -1).length`}, "3"},
		{"limit 0", []string{`"abc".split("", 0).length`}, "0"},
	})
}

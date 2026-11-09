package engine

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonParseGo(t *testing.T, r *Realm, text string) Value {
	t.Helper()
	v, err := callMethodErr(r, ObjectValue(r.JSON), "parse", str(text))
	require.NoError(t, err, text)
	return v
}

func jsonStringifyGo(t *testing.T, r *Realm, v Value, extra ...Value) string {
	t.Helper()
	res, err := callMethodErr(r, ObjectValue(r.JSON), "stringify", append([]Value{v}, extra...)...)
	require.NoError(t, err)
	if res.IsUndefined() {
		return "<undefined>"
	}
	return res.AsString().GoString()
}

func TestJSONParseTable(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		text string
		want any
	}{
		{`1`, int64(1)},
		{` -0 `, math.Copysign(0, -1)},
		{`0.1`, 0.1},
		{`1e21`, 1e21},
		{`-1.5E-3`, -0.0015},
		{`123456789012345678`, int64(123456789012345680)},
		{`9007199254740993`, int64(9007199254740992)},
		{`1e400`, math.Inf(1)},
		{`"str"`, "str"},
		{`"\u0041\n\t\"\\\/\b\f\r"`, "A\n\t\"\\/\b\f\r"},
		{`"\ud83d\ude00"`, "😀"},
		{`"\uD83D\uDE00"`, "😀"},
		{`"你好"`, "你好"},
		{`"emoji 😀 raw"`, "emoji 😀 raw"},
		{`true`, true},
		{`false`, false},
		{`null`, nil},
		{`[]`, []any{}},
		{`[1,"a",null,true,[2],{"b":3}]`, []any{int64(1), "a", nil, true, []any{int64(2)}, map[string]any{"b": int64(3)}}},
		{`{}`, map[string]any{}},
		{`{"a":1,"b":{"c":[1,2]}}`, map[string]any{"a": int64(1), "b": map[string]any{"c": []any{int64(1), int64(2)}}}},
		{`{"a":1,"a":2}`, map[string]any{"a": int64(2)}},
		{`{"__proto__":1}`, map[string]any{"__proto__": int64(1)}},
		{`{"1":"one","0":"zero","x":"y"}`, map[string]any{"0": "zero", "1": "one", "x": "y"}},
		{"\t\n\r {\"a\" : [ 1 , 2 ] } \n", map[string]any{"a": []any{int64(1), int64(2)}}},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			got := r.ToGo(jsonParseGo(t, r, c.text))
			assert.Equal(t, c.want, got)
		})
	}
	// -0 stays -0 as a JS value.
	nz := jsonParseGo(t, r, "-0")
	assert.True(t, SameValue(nz, NumberValue(negativeZero)))
	// Lone surrogate escapes survive as code units.
	lone := jsonParseGo(t, r, `"\ud800x"`)
	assert.Equal(t, []uint16{0xD800, 'x'}, lone.AsString().UTF16())
	// Lone surrogates in UTF-16 input text survive too.
	loneText := FromUTF16([]uint16{'"', 0xDC00, '"'})
	v, err := callMethodErr(r, ObjectValue(r.JSON), "parse", StringValue(loneText))
	require.NoError(t, err)
	assert.Equal(t, []uint16{0xDC00}, v.AsString().UTF16())
	// Key order follows the text (index keys first, ascending).
	o := jsonParseGo(t, r, `{"z":1,"a":2,"10":3,"2":4}`).AsObject()
	assert.Equal(t, []string{"2", "10", "z", "a"}, keyNames(o.OwnEnumerableStringKeys()))
	// Arrays are dense.
	arr := jsonParseGo(t, r, `[1,2,3]`).AsObject()
	assert.True(t, arr.IsDenseArray())
	assert.Same(t, r.ArrayPrototype, arr.Proto())
	// Non-string input is coerced.
	n, err := callMethodErr(r, ObjectValue(r.JSON), "parse", IntValue(42))
	require.NoError(t, err)
	assert.Equal(t, IntValue(42), n)
}

func TestJSONParseErrors(t *testing.T) {
	r := NewRealm()
	cases := []struct{ text, msg string }{
		{``, "Unexpected end of JSON input"},
		{`{`, "Unexpected end of JSON input"},
		{`[1,`, "Unexpected end of JSON input"},
		{`{"a":1,}`, "Unexpected token } in JSON at position 7"},
		{`[1,]`, "Unexpected token ] in JSON at position 3"},
		{`01`, "Unexpected token 1 in JSON at position 1"},
		{`1.`, "Unexpected end of JSON input"},
		{`.5`, "Unexpected token . in JSON at position 0"},
		{`+1`, "Unexpected token + in JSON at position 0"},
		{`1e`, "Unexpected end of JSON input"},
		{`'a'`, "Unexpected token ' in JSON at position 0"},
		{`"a`, "Unterminated string in JSON at position 2"},
		{"\"a\nb\"", "Bad control character in string literal in JSON at position 2"},
		{`"\x"`, "Bad escaped character in JSON at position 2"},
		{`"\u12"`, "Unexpected end of JSON input"},
		{`"\u12g4"`, "Bad Unicode escape in JSON at position 5"},
		{`tru`, "Unexpected end of JSON input"},
		{`trux`, "Unexpected token x in JSON at position 3"},
		{`nul`, "Unexpected end of JSON input"},
		{`undefined`, "Unexpected token u in JSON at position 0"},
		{`NaN`, "Unexpected token N in JSON at position 0"},
		{`1 2`, "Unexpected token 2 in JSON at position 2"},
		{`{1:2}`, "Unexpected token 1 in JSON at position 1"},
		{`{"a" 1}`, "Unexpected token 1 in JSON at position 5"},
		{`[1 2]`, "Unexpected token 2 in JSON at position 3"},
		{`{"a":1 "b":2}`, `Unexpected token " in JSON at position 7`},
		{`é`, "Unexpected token é in JSON at position 0"},
	}
	for _, c := range cases {
		t.Run(c.text[:min(len(c.text), 20)], func(t *testing.T) {
			_, err := callMethodErr(r, ObjectValue(r.JSON), "parse", str(c.text))
			assertErrorKind(t, err, KindSyntaxError, c.msg)
		})
	}
	// Nesting is bounded by jsonMaxDepth, with a RangeError.
	deep := strings.Repeat("[", jsonMaxDepth) + strings.Repeat("]", jsonMaxDepth)
	jsonParseGo(t, r, deep)
	for _, text := range []string{strings.Repeat("[", jsonMaxDepth+1), strings.Repeat(`{"a":`, jsonMaxDepth+1)} {
		_, err := callMethodErr(r, ObjectValue(r.JSON), "parse", str(text))
		assertErrorKind(t, err, KindRangeError, "JSON nesting is too deep (more than 10000 levels)")
	}
	// Interrupts are honoured.
	r.Interrupt("stop")
	_, err := callMethodErr(r, ObjectValue(r.JSON), "parse", str("["+strings.Repeat("1,", 5000)+"1]"))
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
}

func TestJSONParseSharesShapes(t *testing.T) {
	r := NewRealm()
	a := jsonParseGo(t, r, `{"model":"m","prompt":"p","size":1}`).AsObject()
	b := jsonParseGo(t, r, `{"model":"x","prompt":"y","size":2}`).AsObject()
	c := jsonParseGo(t, r, `{"prompt":"y","model":"x","size":2}`).AsObject()
	assert.Same(t, a.Shape(), b.Shape(), "same key order shares one *Shape")
	assert.NotSame(t, a.Shape(), c.Shape(), "different key order is a different shape")
	// Same shape as FromGo for the same (sorted) key set.
	g, err := r.FromGo(map[string]any{"model": "m", "prompt": "p", "size": 1})
	require.NoError(t, err)
	assert.Same(t, a.Shape(), g.AsObject().Shape(), "JSON.parse and FromGo share the realm shape tree")
	// Nested objects share too.
	n1 := jsonParseGo(t, r, `{"a":{"x":1,"y":2}}`).AsObject()
	n2 := jsonParseGo(t, r, `{"a":{"x":3,"y":4}}`).AsObject()
	ax, _ := n1.GetProp(r, key(r, "a"))
	bx, _ := n2.GetProp(r, key(r, "a"))
	assert.Same(t, ax.AsObject().Shape(), bx.AsObject().Shape())
	// Objects remain ordinary: extensible, writable, correct prototype.
	assert.Same(t, r.ObjectPrototype, a.Proto())
	require.NoError(t, a.SetProp(r, key(r, "extra"), IntValue(1)))
	assert.Equal(t, []string{"model", "prompt", "size", "extra"}, keyNames(a.OwnEnumerableStringKeys()))
	// Objects with more than 64 keys still work (dictionary mode).
	var sb strings.Builder
	sb.WriteString("{")
	for i := range 70 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"k` + NumberToGoString(float64(i)) + `":` + NumberToGoString(float64(i)))
	}
	sb.WriteString("}")
	big := jsonParseGo(t, r, sb.String()).AsObject()
	assert.Len(t, big.OwnEnumerableStringKeys(), 70)
	kv, _ := big.GetProp(r, key(r, "k69"))
	assert.Equal(t, IntValue(69), kv)
}

func TestJSONParseReviver(t *testing.T) {
	r := NewRealm()
	var visits []string
	reviver := ObjectValue(r.NewNativeFunction(AtomEmpty, 2, func(r *Realm, this Value, args []Value) (Value, error) {
		k := args[0].AsString().GoString()
		visits = append(visits, k)
		v := args[1]
		if v.IsNumber() {
			return NumberValue(v.AsNumber() * 10), nil
		}
		if k == "drop" {
			return Undefined(), nil
		}
		// holder is `this`
		if !this.IsObject() {
			return Undefined(), r.TypeError("holder is not an object")
		}
		return v, nil
	}))
	v, err := callMethodErr(r, ObjectValue(r.JSON), "parse", str(`{"a":1,"b":[2,{"c":3}],"drop":"x","s":"t"}`), reviver)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": int64(10), "b": []any{int64(20), map[string]any{"c": int64(30)}}, "s": "t"}, r.ToGo(v))
	assert.Equal(t, []string{"a", "0", "c", "1", "b", "drop", "s", ""}, visits)
	// The root holder is {"": value}; returning undefined at the root yields undefined.
	drop := ObjectValue(r.NewNativeFunction(AtomEmpty, 2, func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil }))
	v, err = callMethodErr(r, ObjectValue(r.JSON), "parse", str(`[1]`), drop)
	require.NoError(t, err)
	assert.True(t, v.IsUndefined())
	// Errors in the reviver propagate.
	thrower := ObjectValue(r.NewNativeFunction(AtomEmpty, 2, func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.RangeError("nope")
	}))
	_, err = callMethodErr(r, ObjectValue(r.JSON), "parse", str(`1`), thrower)
	assertErrorKind(t, err, KindRangeError, "nope")
}

// TestJSONParseReviverGrowing: a reviver that adds a container per level
// makes the walk endless; it ends in a RangeError instead of overflowing the
// Go stack, while the deepest structure the parse accepts is still revived.
func TestJSONParseReviverGrowing(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"growing", `return JSON.parse('[1,[2]]', function (k, v) { if (k === '0') this[1] = [5, 6]; return v })`, "!RangeError: JSON nesting is too deep (more than 10000 levels)"},
		{"growing object", `return JSON.parse('{"a":1,"b":{}}', function (k, v) { if (k === 'a') this.b = {a: 1, b: {}}; return v })`, "!RangeError: JSON nesting is too deep (more than 10000 levels)"},
		{"deepest", `const s = '['.repeat(10000) + ']'.repeat(10000); let n = 0; JSON.parse(s, function (k, v) { n++; return v }); return n + (() => { try { JSON.parse('[' + s + ']') } catch (e) { return e.name } })()`, "10000RangeError"},
	})
}

func TestJSONStringifyTable(t *testing.T) {
	r := NewRealm()
	mk := func(src string) Value { return jsonParseGo(t, r, src) }
	fn := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil }))
	withFn := r.NewObject()
	require.NoError(t, withFn.SetProp(r, key(r, "f"), fn))
	require.NoError(t, withFn.SetProp(r, key(r, "u"), Undefined()))
	require.NoError(t, withFn.SetProp(r, key(r, "sym"), SymbolValue(SymIterator)))
	require.NoError(t, withFn.SetProp(r, key(r, "n"), Null()))
	arrWithFn := r.NewArray(fn, Undefined(), SymbolValue(SymIterator), NaN(), NumberValue(math.Inf(-1)))
	nested := r.NewObject()
	require.NoError(t, nested.SetProp(r, key(r, "holes"), ObjectValue(r.NewArrayLen(2))))
	numObj, _ := r.ToObject(IntValue(5))
	strObj, _ := r.ToObject(str("s"))
	boolObj, _ := r.ToObject(True())
	dateObj, err := r.Construct(ObjectValue(r.DateCtor), []Value{IntValue(0)}, nil)
	require.NoError(t, err)
	cases := []struct {
		name string
		v    Value
		want string
	}{
		{"number", IntValue(1), "1"},
		{"negative zero", NumberValue(negativeZero), "0"},
		{"float", NumberValue(0.1), "0.1"},
		{"1e21", NumberValue(1e21), "1e+21"},
		{"large int", NumberValue(123456789012345680), "123456789012345680"},
		{"NaN", NaN(), "null"},
		{"Infinity", NumberValue(math.Inf(1)), "null"},
		{"string", str("a\"b\\c\n\t\u0001"), `"a\"b\\c\n\t\u0001"`},
		{"string unicode", str("你好😀"), `"你好😀"`},
		{"lone surrogate", utf16Str('a', 0xD800, 'b', 0xDFFF), `"a\ud800b\udfff"`},
		{"2028 unescaped", str("\u2028"), "\"\u2028\""},
		{"true", True(), "true"},
		{"null", Null(), "null"},
		{"undefined", Undefined(), "<undefined>"},
		{"function", fn, "<undefined>"},
		{"symbol", SymbolValue(SymIterator), "<undefined>"},
		{"empty object", ObjectValue(r.NewObject()), "{}"},
		{"empty array", ObjectValue(r.NewArrayLen(0)), "[]"},
		{"object", mk(`{"a":1,"b":"x","c":[true,null],"d":{"e":{}}}`), `{"a":1,"b":"x","c":[true,null],"d":{"e":{}}}`},
		{"omits undefined function symbol", ObjectValue(withFn), `{"n":null}`},
		{"array nulls", ObjectValue(arrWithFn), `[null,null,null,null,null]`},
		{"array holes", ObjectValue(nested), `{"holes":[null,null]}`},
		{"number wrapper", ObjectValue(numObj), "5"},
		{"string wrapper", ObjectValue(strObj), `"s"`},
		{"boolean wrapper", ObjectValue(boolObj), "true"},
		{"date toJSON", dateObj, `"1970-01-01T00:00:00.000Z"`},
		{"index keys first", mk(`{"b":1,"2":2,"a":3,"1":4}`), `{"1":4,"2":2,"b":1,"a":3}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, jsonStringifyGo(t, r, c.v))
		})
	}
}

func TestJSONStringifyIndentAndReplacer(t *testing.T) {
	r := NewRealm()
	v := jsonParseGo(t, r, `{"a":1,"b":[1,{"c":2}],"d":{},"e":[]}`)
	assert.Equal(t, "{\n  \"a\": 1,\n  \"b\": [\n    1,\n    {\n      \"c\": 2\n    }\n  ],\n  \"d\": {},\n  \"e\": []\n}", jsonStringifyGo(t, r, v, Undefined(), IntValue(2)))
	assert.Equal(t, "{\n\t\"a\": 1\n}", jsonStringifyGo(t, r, jsonParseGo(t, r, `{"a":1}`), Undefined(), str("\t")))
	// Indent clamps to 10 characters / 10 spaces; 0 or "" means none.
	assert.Equal(t, "[\n          1\n]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), IntValue(20)))
	assert.Equal(t, "[\n01234567891\n]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), str("0123456789abc")))
	assert.Equal(t, "[1]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), IntValue(0)))
	assert.Equal(t, "[1]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), str("")))
	assert.Equal(t, "[\n x1\n]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), str(" x")))
	numObj, _ := r.ToObject(IntValue(1))
	assert.Equal(t, "[\n 1\n]", jsonStringifyGo(t, r, jsonParseGo(t, r, `[1]`), Undefined(), ObjectValue(numObj)))
	// Replacer array selects and orders keys (duplicates and non-strings dropped; numbers allowed), applies to nested objects.
	list := r.NewArray(str("b"), str("a"), str("b"), IntValue(1), True(), str("zz"))
	assert.Equal(t, `{"b":[1,{}],"a":1}`, jsonStringifyGo(t, r, v, ObjectValue(list)))
	withIdx := jsonParseGo(t, r, `{"1":"one","a":{"1":"x","b":2}}`)
	assert.Equal(t, `{"a":{"1":"x"},"1":"one"}`, jsonStringifyGo(t, r, withIdx, ObjectValue(r.NewArray(str("a"), IntValue(1)))))
	// Replacer function sees (key, value) with the holder as this and can rewrite/omit.
	repl := ObjectValue(r.NewNativeFunction(AtomEmpty, 2, func(r *Realm, this Value, args []Value) (Value, error) {
		k := args[0].AsString().GoString()
		if k == "a" {
			return str("A"), nil
		}
		if k == "d" {
			return Undefined(), nil
		}
		if args[1].IsNumber() && k != "" {
			return NumberValue(args[1].AsNumber() + 100), nil
		}
		return args[1], nil
	}))
	assert.Equal(t, `{"a":"A","b":[101,{"c":102}],"e":[]}`, jsonStringifyGo(t, r, v, repl))
	// Root call: key "" and holder {"": value}.
	var rootThis Value
	rootRepl := ObjectValue(r.NewNativeFunction(AtomEmpty, 2, func(r *Realm, this Value, args []Value) (Value, error) {
		if args[0].AsString().Len() == 0 {
			rootThis = this
		}
		return args[1], nil
	}))
	jsonStringifyGo(t, r, IntValue(1), rootRepl)
	require.True(t, rootThis.IsObject())
	rv, _ := rootThis.AsObject().GetProp(r, StringKey(AtomEmpty))
	assert.Equal(t, IntValue(1), rv)
}

// TestJSONStringifyStackLevels: the stringifier's stack keeps its first
// levels in an array of its own and the rest in a slice; a cycle closing on
// an ancestor at either side of that boundary and of jsonScanDepth (past
// which the deep set finds it) is found as it closes, a shared object that
// closes none is not a cycle, and the indentation follows the depth on both
// sides.
func TestJSONStringifyStackLevels(t *testing.T) {
	f := evalModule(t, `
export function chain(n, k, shared) {
	const all = [];
	let cur = {};
	const root = cur;
	all.push(cur);
	for (let i = 1; i < n; i++) { const next = {}; cur.c = next; cur = next; all.push(cur); }
	let gets = 0;
	if (k >= 0) Object.defineProperty(cur, "back", { get() { gets++; return all[k]; }, enumerable: true });
	if (shared) { const leaf = {v: 1}; cur.a = leaf; cur.b = [leaf, leaf]; }
	try { return String(JSON.stringify(root).length); } catch (e) { return e.name + "," + gets; }
}
export function indent(n) {
	let v = 0;
	for (let i = 0; i < n; i++) v = [v];
	const line = JSON.stringify(v, null, 1).split("\n")[n];
	return line.length - line.trimStart().length;
}`)
	for _, n := range []int{1, 15, 16, 17, 18, jsonScanDepth - 1, jsonScanDepth, jsonScanDepth + 1, jsonScanDepth + 40} {
		for _, k := range []int{0, 14, 15, 16, 17, jsonScanDepth - 1, jsonScanDepth, n - 1} {
			if k >= n {
				continue
			}
			assert.Equal(t, "TypeError,1", f.call("chain", n, k, false), "a cycle from level %d to %d, found when it closes", n-1, k)
		}
		ok := f.call("chain", n, -1, true)
		assert.NotContains(t, ok, "Error", "%d levels and a shared leaf", n)
		assert.Equal(t, int64(n), f.call("indent", n), "indentation at %d levels", n)
	}
}

func TestJSONStringifyToJSONAndErrors(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	toJSON := ObjectValue(r.NewNativeFunction(AtomToJSON, 1, func(r *Realm, this Value, args []Value) (Value, error) {
		return str("key=" + args[0].AsString().GoString()), nil
	}))
	require.NoError(t, o.SetProp(r, StringKey(AtomToJSON), toJSON))
	holder := r.NewObject()
	require.NoError(t, holder.SetProp(r, key(r, "x"), ObjectValue(o)))
	assert.Equal(t, `{"x":"key=x"}`, jsonStringifyGo(t, r, ObjectValue(holder)))
	assert.Equal(t, `"key="`, jsonStringifyGo(t, r, ObjectValue(o)))
	arr := r.NewArray(ObjectValue(o))
	assert.Equal(t, `["key=0"]`, jsonStringifyGo(t, r, ObjectValue(arr)))
	// Cycle detection.
	cyc := r.NewObject()
	require.NoError(t, cyc.SetProp(r, key(r, "self"), ObjectValue(cyc)))
	_, err := callMethodErr(r, ObjectValue(r.JSON), "stringify", ObjectValue(cyc))
	assertErrorKind(t, err, KindTypeError, "Converting circular structure to JSON")
	cycArr := r.NewArrayLen(0)
	require.True(t, cycArr.Push(r, ObjectValue(cycArr)))
	_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", ObjectValue(cycArr))
	assertErrorKind(t, err, KindTypeError, "Converting circular structure to JSON")
	// Shared (non-cyclic) references are fine.
	leaf := r.NewObject()
	twice := r.NewArray(ObjectValue(leaf), ObjectValue(leaf))
	assert.Equal(t, `[{},{}]`, jsonStringifyGo(t, r, ObjectValue(twice)))
	// BigInt throws.
	b, _ := NewBigIntFromDecimal("1")
	_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", BigIntValue(b))
	assertErrorKind(t, err, KindTypeError, "Do not know how to serialize a BigInt")
	// Getters run and can throw.
	thrower := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.TypeError("getter boom")
	})
	g := r.NewObject()
	g.DefineOwnAccessorFast(r, key(r, "g"), thrower, nil, attrEnumerable|attrConfigurable)
	_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", ObjectValue(g))
	assertErrorKind(t, err, KindTypeError, "getter boom")
	// Nesting up to jsonMaxDepth serializes.
	text := strings.Repeat("[", jsonMaxDepth) + strings.Repeat("]", jsonMaxDepth)
	assert.Equal(t, text, jsonStringifyGo(t, r, jsonParseGo(t, r, text)))
	// Interrupt.
	bigArr := jsonParseGo(t, r, "["+strings.Repeat("1,", 5000)+"1]")
	r.Interrupt("stop")
	_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", bigArr)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
}

// TestJSONStringifySkipsCountWork checks that a property JSON.stringify
// visits and does not write counts toward the interrupt check like one it
// writes: a key of the property list the object lacks, an undefined,
// function or symbol value, a non-enumerable or symbol key, the last also
// where the key walk drops it (a dictionary-mode object, a proxy). Each
// input writes a few dozen values and skips thousands, so it must stop at
// the pending interrupt.
func TestJSONStringifySkipsCountWork(t *testing.T) {
	f := evalModule(t, `
const keys = Array.from({length: 1000}, (_, i) => "k" + i);
const many = {};
for (const k of keys) many[k] = undefined;
const values = {a: undefined, b() {}, c: Symbol()};
const hidden = {[Symbol()]: 1};
for (const k of ["d", "e", "f", "g"]) Object.defineProperty(hidden, k, {value: 1});
const dictHidden = {}, dictSymbols = {};
for (const k of keys) Object.defineProperty(dictHidden, k, {value: 1});
for (const k of keys) dictSymbols[Symbol(k)] = 1;
export const cases = {
  list: [new Array(10).fill({}), keys],
  "list of a proxy": [new Array(10).fill(new Proxy({}, {})), new Proxy(keys, {})],
  "many undefined values": [new Array(10).fill(many)],
  "skipped values": [new Array(1500).fill(values)],
  "skipped keys": [new Array(1200).fill(hidden)],
  "non-enumerable keys of a dictionary": [new Array(10).fill(dictHidden)],
  "symbol keys of a dictionary": [new Array(10).fill(dictSymbols)],
  "non-enumerable keys through a proxy": [new Array(10).fill(new Proxy(dictHidden, {}))],
};
`)
	cases, ok := f.env.GetBindingValue("cases")
	require.True(t, ok)
	r := f.r
	for _, name := range []string{"list", "list of a proxy", "many undefined values", "skipped values", "skipped keys",
		"non-enumerable keys of a dictionary", "symbol keys of a dictionary", "non-enumerable keys through a proxy"} {
		c, err := cases.AsObject().GetProp(r, key(r, name))
		require.NoError(t, err)
		args, err := r.CreateListFromArrayLike(c)
		require.NoError(t, err)
		_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", args...)
		require.NoError(t, err, name)
		r.Interrupt("stop")
		_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", args...)
		var ie *InterruptedError
		assert.ErrorAs(t, err, &ie, name)
		r.ClearInterrupt()
	}
}

// TestJSONStringifyChecksLengthAsItWrites checks that JSON.stringify stops
// at the string length limit as it writes, not at the end: a hole costs no
// memory, so 2**28 of them wrote about 1.3 GB before the final check, and
// references to one object repeat its output. With the limit at 1000 each
// input passes it within a few hundred values, so the RangeError has to come
// before the 4096th unit of work (a value written or a key walked) checks
// the pending interrupt: the object's 3000 keys leave about 1100 values.
func TestJSONStringifyChecksLengthAsItWrites(t *testing.T) {
	lowerMaxStringLength(t, 1000)
	f := evalModule(t, `
const holes = []; holes.length = 2 ** 28;
const empties = {};
for (let i = 0; i < 3000; i++) empties["k" + i] = [];
export const cases = {
  holes: [holes],
  "holes indented": [[holes], null, 10],
  "undefined elements": [new Array(5000).fill(undefined)],
  "function elements": [new Array(5000).fill(() => {})],
  "empty arrays": [new Array(5000).fill([])],
  "empty objects": [new Array(5000).fill({})],
  "object of empty arrays": [empties],
};
export {holes};
`)
	cases, ok := f.env.GetBindingValue("cases")
	require.True(t, ok)
	r := f.r
	r.Interrupt("stop")
	defer r.ClearInterrupt()
	for _, name := range []string{"holes", "holes indented", "undefined elements", "function elements", "empty arrays", "empty objects", "object of empty arrays"} {
		c, err := cases.AsObject().GetProp(r, key(r, name))
		require.NoError(t, err)
		args, err := r.CreateListFromArrayLike(c)
		require.NoError(t, err)
		_, err = callMethodErr(r, ObjectValue(r.JSON), "stringify", args...)
		assertInvalidLength(t, err, name)
	}
	holes, ok := f.env.GetBindingValue("holes")
	require.True(t, ok)
	_, err := r.JSONStringify(holes)
	assertInvalidLength(t, err, "JSONStringify")
}

func assertInvalidLength(t *testing.T, err error, name string) {
	t.Helper()
	var exc *Exception
	if assert.ErrorAs(t, err, &exc, name) {
		assert.Equal(t, "RangeError: Invalid string length", errorDisplayString(exc.Value), name)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	r := NewRealm()
	for _, text := range []string{
		`{"model":"video-gen-1","prompt":"a cat surfing on a rainbow","size":"1280x720","duration":8,"seed":42,"metadata":{"user":"u_1","tags":["a","b","c"],"flags":{"hd":true,"audio":false}}}`,
		`[1,2.5,-3,1e-7,1e+21,"x",null,true,false,{"a":[[]]}]`,
		`"\u0000\u001f\"\\"`,
		`{"你好":"世界","emoji":"😀","lone":"\ud800"}`,
		`0.1`, `-0.5`, `123456789`, `1.7976931348623157e+308`, `5e-324`,
	} {
		v := jsonParseGo(t, r, text)
		assert.Equal(t, text, jsonStringifyGo(t, r, v), "round trip of %s", text)
	}
	// Go-callable entry points.
	v, err := r.JSONParse(FromGoString(`{"a":[1,2]}`))
	require.NoError(t, err)
	s, err := r.JSONStringify(v)
	require.NoError(t, err)
	assert.Equal(t, `{"a":[1,2]}`, s.GoString())
	s, err = r.JSONStringify(Undefined())
	require.NoError(t, err)
	assert.Nil(t, s)
	_, err = r.JSONParse(FromGoString(`{`))
	assertErrorKind(t, err, KindSyntaxError, "Unexpected end of JSON input")
}

// TestJSONStringifyPlainString: JSON.stringify of a long ASCII string with
// nothing to escape is the string between quotation marks without a copy
// of its text, and a serializer's escaping of a text into a script,
// JSON.stringify(s).slice(1, -1), gives s itself; a string with something
// to escape, a short one and a UTF-16 one are quoted as before.
func TestJSONStringifyPlainString(t *testing.T) {
	r := NewRealm()
	long := strings.Repeat("<div class=x>text</div>", 20)
	s := FromGoString(long)
	q := callJSON(t, r, "stringify", StringValue(s)).AsString()
	got, err := callMethodErr(r, StringValue(q), "slice", IntValue(1), IntValue(-1))
	require.NoError(t, err)
	assert.Same(t, s, got.AsString(), "the text itself")
	assert.Equal(t, `"`+long+`"`, q.GoString())
	rope := concat(FromGoString(long), FromGoString(long))
	assert.Equal(t, `"`+long+long+`"`, callJSON(t, r, "stringify", StringValue(rope)).AsString().GoString())
	for _, v := range []string{"short", strings.Repeat("a", 70) + "\"q\"\n\\", strings.Repeat("é", 70) + "\"", strings.Repeat("ab", 40) + "\x01"} {
		want, _ := json.Marshal(v)
		assert.Equal(t, string(want), callJSON(t, r, "stringify", str(v)).AsString().GoString(), "%q", v)
	}
	assert.Equal(t, `"`+long+`"`, runScriptsIn(t, r, `JSON.stringify(`+"`"+long+"`"+`, null, 2)`))
	assert.Equal(t, `"X"`, runScriptsIn(t, r, `JSON.stringify('`+long+`', () => 'X')`), "a replacer function still runs")
	assert.Equal(t, "true", runScriptsIn(t, r, `var s = 'q'.repeat(100); JSON.stringify(s).slice(1, -1) === s`))
}

func callJSON(t *testing.T, r *Realm, name string, args ...Value) Value {
	t.Helper()
	v, err := callMethodErr(r, ObjectValue(r.JSON), name, args...)
	require.NoError(t, err)
	return v
}

// TestJSONStringifyRopeShapes: the rope JSON.stringify returns for a plain
// string behaves in every use as the flat string of the same text, for a
// text that holds its units and for one that is a slice of a longer
// string; slices across its children included.
func TestJSONStringifyRopeShapes(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "same", runScriptsIn(t, r, `
const e = "<p class=a>" + "x".repeat(90) + "</p>";
const big = "z".repeat(5000) + e + "z".repeat(5000);
const out = [];
for (const v of [e, big.slice(5000, 5000 + e.length), "x".repeat(64)]) {
  const R = () => JSON.stringify(v), F = ['"', v, '"'].join("");
  const ops = [
    s => s.length, s => s === F, s => s == F, s => s.charAt(7), s => s.charCodeAt(0), s => s.codePointAt(3), s => s.at(-1),
    s => s.indexOf("x"), s => s.lastIndexOf("x"), s => s.includes("x</"), s => s.startsWith('"<p'), s => s.endsWith('p>"'),
    s => s.slice(0, 10), s => s.slice(1, -1), s => s.slice(5, 20), s => s.slice(-6), s => s.slice(1, 2), s => s.slice(-2, -1),
    s => s.substring(3, 40), s => s.substr(2, 5), s => s.split("x").length, s => s.split("").length, s => s.replace(/x+/, "y"),
    s => s.replaceAll("x", ""), s => s.toUpperCase(), s => s.toLowerCase(), s => s.trim(), s => s.padStart(130, "-"),
    s => s.padEnd(130, "-"), s => s.repeat(2), s => s.concat("!"), s => s + "!", s => "!" + s, s => s.localeCompare(F),
    s => s.normalize("NFC"), s => s.match(/x+/)[0].length, s => [...s].length, s => encodeURIComponent(s), s => JSON.parse(s),
    s => JSON.stringify(s), s => JSON.stringify({k: s}), s => { const o = {}; o[s] = 1; return o[F] + Object.keys(o)[0]; },
    s => new Map([[s, 1]]).get(F), s => new Set([s, F]).size, s => s.isWellFormed(), s => `+"`${s}`"+`, s => String(s) === F,
    s => s < F || s > F, s => s.search(/p>/), s => [s, s].join(), s => s.slice(0, 10) + s.slice(10) === F,
    s => s.slice(1, -1).slice(2, 30), s => s.slice(1, -1) === v, s => ({[s]: 2})[F], s => Symbol.for(s) === Symbol.for(F),
  ];
  for (const op of ops) {
    const a = op(R()), b = op(F);
    if (a !== b) out.push(op + ": " + a + " / " + b);
  }
}
out.join("\n") || "same"`))
}

// TestJSONStringifyPlainStringDepth: a serializer's escaping of a text,
// run again on what it gave, gives the text itself each time, so these ropes do not
// nest; quoting such a rope again has quotation marks to escape and is
// flat.
func TestJSONStringifyPlainStringDepth(t *testing.T) {
	r := NewRealm()
	s := FromGoString(strings.Repeat("<li>item</li>", 10))
	esc := evalValue(t, r, `(s => JSON.stringify(s).slice(1, -1))`)
	v := StringValue(s)
	for range 10 {
		var err error
		v, err = r.Call(esc, Undefined(), []Value{v})
		require.NoError(t, err)
	}
	assert.Same(t, s, v.AsString())
	q := callJSON(t, r, "stringify", StringValue(s))
	require.True(t, isRopeForTest(q.AsString()))
	qq := callJSON(t, r, "stringify", q).AsString()
	assert.False(t, isRopeForTest(qq))
	assert.Equal(t, `"\"`+s.GoString()+`\""`, qq.GoString())
}

// TestJSONStringifyPlainStringCharges: under a memory limit, quoting a
// long plain string charges two rope headers, not a copy of the text; a
// flattening of the result charges its copy, as for any rope.
func TestJSONStringifyPlainStringCharges(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(1 << 30)
	s := FromGoString(strings.Repeat("abcd", 1<<14))
	before := r.Stats().AllocatedBytes
	q := callJSON(t, r, "stringify", StringValue(s)).AsString()
	quoted := r.Stats().AllocatedBytes - before
	assert.Positive(t, quoted)
	assert.Less(t, quoted, int64(512), "the rope headers")
	q.flatten()
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-before, int64(s.Len()+2), "the flat copy")
}

// TestJSONEscapesExact: jsonEscapes flags exactly the bytes JSON quoting
// escapes and the control characters among them, whatever the bytes next
// to them, so that jsonQuotedLen can count them.
func TestJSONEscapesExact(t *testing.T) {
	near := []byte{0, 1, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x1f, 0x20, 0x21, '"', '#', '\\', ']', 'a', 0x7f, 0x80, 0xa2, 0xdc, 0xff}
	rng := rand.New(rand.NewPCG(7, 8))
	var w [8]byte
	for range 200000 {
		for i := range w {
			if rng.IntN(4) == 0 {
				w[i] = byte(rng.Uint32())
			} else {
				w[i] = near[rng.IntN(len(near))]
			}
		}
		all, ctl := jsonEscapes(binary.LittleEndian.Uint64(w[:]))
		for i, c := range w {
			esc := c < 0x20 || c == '"' || c == '\\'
			require.Equal(t, esc, all>>(8*i+7)&1 == 1, "% x byte %d", w, i)
			require.Equal(t, c < 0x20, ctl>>(8*i+7)&1 == 1, "% x byte %d", w, i)
		}
	}
}

// TestJSONStringifyStringSized: JSON.stringify of a string that is not a
// plain long one writes its quoting into a buffer of exactly its length,
// one allocation for an output of up to smallASCIIMax bytes and the units
// and the header past it, with the output of the general path; it leaves
// the size hint of the other values' outputs as it was.
// TestJSONStringifyStringLimit: the quoting of a string past the length
// limit throws, whether it is a plain rope, escaped, UTF-16, or has \u
// escapes of controls or lone surrogates; one at the limit does not.
func TestJSONStringifyStringLimit(t *testing.T) {
	old := maxStringLength
	maxStringLength = 200
	defer func() { maxStringLength = old }()
	r := NewRealm()
	for _, c := range []struct {
		src string
		ok  bool
	}{
		{`"x".repeat(199)`, false}, // plain: 201
		{`"x".repeat(198)`, true},
		{`"\n".repeat(100)`, false}, // escaped: 202
		{`"\n".repeat(99)`, true},
		{`"é".repeat(199)`, false}, // UTF-16: 201
		{`"é".repeat(198)`, true},
		{`"\u0001".repeat(34)`, false}, // \u escapes: 206
		{`"\u0001".repeat(33)`, true},
		{`"\ud800".repeat(34)`, false}, // lone surrogates: 206
		{`"\ud800".repeat(33)`, true},
		{`"x".repeat(150) + "\n".repeat(25)`, false}, // 202
		{`"x".repeat(150) + "\n".repeat(24)`, true},
	} {
		out := runScriptsIn(t, r, "JSON.stringify("+c.src+").length")
		if c.ok {
			assert.Equal(t, "200", out, c.src)
		} else {
			assert.True(t, strings.HasPrefix(out, "RangeError: Invalid string length"), "%s: %s", c.src, out)
		}
	}
}

func TestJSONStringifyStringSized(t *testing.T) {
	r := NewRealm()
	quoteAll := evalValue(t, r, `(s => JSON.stringify([s]).slice(1, -1))`) // the general path
	rng := rand.New(rand.NewPCG(9, 10))
	pick := []string{`a`, `z`, ` `, `\"`, `\\`, `\n`, `\t`, `\x01`, `\x1f`, `\x7f`, `é`, `€`, `😀`, `\ud800`, `\udc00`, `<`, `\u2028`}
	for k := range 3000 {
		var b strings.Builder
		n := rng.IntN(200)
		if k%10 == 0 {
			n = rng.IntN(5000)
		}
		ascii := k%2 == 0
		for range n {
			p := pick[rng.IntN(len(pick))]
			if ascii && (p[0] >= 0x80 || strings.HasPrefix(p, `\u`)) {
				p = "q"
			}
			b.WriteString(p)
		}
		src := `"` + b.String() + `"`
		v := evalValue(t, r, src)
		if k%3 == 0 && v.AsString().Len() > 2 {
			h := v.AsString().Len() / 2
			v = StringValue(concat(v.AsString().Substring(0, h), v.AsString().Substring(h, v.AsString().Len())))
		}
		got := callJSON(t, r, "stringify", v).AsString()
		want, err := r.Call(quoteAll, Undefined(), []Value{v})
		require.NoError(t, err)
		require.True(t, got.Equals(want.AsString()), "%s: %q / %q", src, got.GoString(), want.AsString().GoString())
		if got.kind == strRope {
			continue // plain: TestJSONStringifyPlainString
		}
		f := v.AsString()
		if f.kind == strRope {
			f = f.flat(new(String))
		}
		n64, plain := jsonQuotedLen(f)
		require.Equal(t, int64(got.Len()), n64)
		require.Equal(t, plain, f.kind == strASCII && got.Len() == f.Len()+2)
	}
	// One allocation up to smallASCIIMax bytes, the bytes or units and the
	// header past it, of the output's size.
	hint := r.jsonSizeHint
	for _, c := range []struct {
		s      string
		allocs float64
	}{
		{"short", 1},
		{strings.Repeat("a\"", 20), 1},
		{strings.Repeat("a\"", 21), 2},
		{strings.Repeat("ab\n", 3000), 2},
		{strings.Repeat("é\x01", 3000), 2},
	} {
		v := StringValue(FromGoString(c.s))
		args := []Value{v}
		n := callJSON(t, r, "stringify", v).AsString().Len()
		assert.Equal(t, c.allocs, testing.AllocsPerRun(20, func() { jsonStringify(r, Undefined(), args) }), "%.20q", c.s)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		for range 20 {
			jsonStringify(r, Undefined(), args)
		}
		runtime.ReadMemStats(&m1)
		size := n
		if FromGoString(c.s).kind == strUTF16 {
			size = 2 * n
		}
		room := size + size/8 + 16 // a size class
		if size > 32<<10 {
			room = (size + 8191) &^ 8191 // pages
		}
		per := int(m1.TotalAlloc-m0.TotalAlloc) / 20
		assert.LessOrEqual(t, per, room+int(stringHeaderSize)+16, "%.20q: %d bytes for %d", c.s, per, size)
	}
	assert.Equal(t, hint, r.jsonSizeHint, "strings leave the size hint")
	assert.Equal(t, `"\u0001\ud800"`, runScriptsIn(t, r, `JSON.stringify("\x01\ud800")`))
}

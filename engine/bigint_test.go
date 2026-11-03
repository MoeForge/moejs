package engine

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MoeForge/moejs/syntax"
)

func TestBigIntOperators(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"arithmetic", `return [1n + 2n, 5n - 7n, 6n * 7n, 7n / 2n, -7n / 2n, 7n % 3n, -7n % 3n, 7n % -3n, 2n ** 10n, (-2n) ** 3n, 0n ** 0n, (-1n) ** 5n, 0n ** 3n].join()`, "3,-2,42,3,-3,1,-1,1,1024,-8,1,-1,0"},
		{"large", `return [2n ** 100n, 2n ** 64n - 1n, (2n ** 64n) * (2n ** 64n) / (2n ** 63n), -(10n ** 30n) % 7n].join()`, "1267650600228229401496703205376,18446744073709551615,36893488147419103232,-1"},
		{"bitwise", `return [5n & 3n, 5n | 3n, 5n ^ 3n, ~5n, ~-1n, -5n & 3n, -5n | 3n, -5n ^ 3n, ~(2n ** 64n)].join()`, "1,7,6,-6,0,3,-5,-8,-18446744073709551617"},
		{"shifts", `return [1n << 10n, 1024n >> 3n, -9n >> 1n, 1n << -1n, 8n >> -2n, -1n >> 100n, 5n >> 100n, 0n << 100000000n, 3n >> 1000000000000n, -1n << -1n, -3n >> 10n ** 30n].join()`, "1024,128,-5,0,32,-1,0,0,0,-1,-1"},
		{"update", `let a = 1n; a++; ++a; let b = a--; const o = {x: 1n}; o.x++; o.x += 2n; return [a, b, -a, typeof -a, o.x, --o.x].join()`, "2,3,-2,bigint,4,3"},
		{"compound assignment", `let x = 10n; x += 5n; x *= 2n; x -= 1n; x /= 3n; x %= 5n; x **= 3n; x <<= 2n; x >>= 1n; x &= 0xffn; x |= 1n; x ^= 2n; return String(x)`, "131"},
		{"string concatenation", "return [1n + \"\", \"x\" + 2n, `${3n}`, 4n + \"5\", 1n + {toString() { return \"s\" }}].join()", "1,x2,3,45,1s"},
		{"objects convert", `return [({valueOf() { return 2n }}) * 3n, Object(5n) + 1n, -Object(2n), ~Object(0n)].join()`, "6,6,-2,-1"},
		{"divide by zero", `return 1n / 0n`, "!RangeError: Division by zero"},
		{"modulo zero", `return 1n % 0n`, "!RangeError: Division by zero"},
		{"negative exponent", `return 2n ** -1n`, "!RangeError: Exponent must be non-negative"},
		{"mix", `return 1n + 1`, "!TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{"mix multiply", `return 2 * 1n`, "!TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{"mix bitwise", `return 1n & 1`, "!TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{"mix unsigned shift", `return 1n >>> 1`, "!TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{"unsigned shift", `return 1n >>> 0n`, "!TypeError: BigInts have no unsigned right shift, use >> instead"},
		{"unary plus", `return +1n`, "!TypeError: Cannot convert a BigInt value to a number"},
		{"Math", `return Math.abs(1n)`, "!TypeError: Cannot convert a BigInt value to a number"},
		{"shift too large", `return 1n << 2000000n`, "!RangeError: Maximum BigInt size exceeded"},
		{"shift huge", `return 1n << (2n ** 64n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"power too large", `return 2n ** 2000000n`, "!RangeError: Maximum BigInt size exceeded"},
		{"power huge", `return 3n ** (2n ** 64n)`, "!RangeError: Maximum BigInt size exceeded"},
		// (bitlen(a)-1)*b is 2^32 and 2^32-2^16: 0 and negative in a 32-bit int.
		{"power bound wraps int32", `return (2n ** 65536n) ** 65536n`, "!RangeError: Maximum BigInt size exceeded"},
		{"power bound wraps int32 negative", `return (2n ** 65536n) ** 65535n`, "!RangeError: Maximum BigInt size exceeded"},
		{"product too large", `return (2n ** 1000000n) * (2n ** 1000000n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"sum too large", `const x = 2n ** 1048575n; return (x + x) + x`, "!RangeError: Maximum BigInt size exceeded"},
		{"largest", `const x = 2n ** 1048575n; return (x - 1n + x).toString(16).length`, "262144"},
	})
}

func TestBigIntComparisons(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"relational", `return [1n < 2n, 2n < 1n, 1n < 1.5, 2n > 1.5, 1n <= 1, 1n >= 1.0000001, 1n < Infinity, -1n > -Infinity, 1n < NaN, 1n > NaN, 10n > "9", "10" > 9n, 1n < "x", 1n >= "x", "0x10" > 15n, 2n ** 64n > 2 ** 64, 2n ** 64n + 1n > 2 ** 64, 1n < "1_0"].join()`, "true,false,true,true,true,false,true,true,false,false,true,true,false,false,true,false,true,false"},
		{"equality", `return [1n == 1, 1n == 1.5, 0n == false, 1n == true, 2n == true, 1n == "1", 1n == " 1 ", 1n == "1n", 1n == "0x1", 0n == "", 1n === 1n, 1n != 2n, 1n == Object(1n), NaN == 0n, 2n ** 64n == 2 ** 64, 2n ** 64n == "18446744073709551616", 1n == "-0x1", 1n === 1].join()`, "true,false,true,true,false,true,true,false,true,true,true,true,true,false,true,true,false,false"},
		{"same value", `return [Object.is(1n, 1n), Object.is(0n, -0n), [1n, 2n].includes(2n), [1n].indexOf(1n), new Set([1n, 1n, 2n]).size, new Map([[1n, "a"]]).get(1n), new Set([2n ** 70n]).has(2n ** 70n), new Map([[1n, 1]]).has(1), [0n].includes(0), [2n ** 64n].lastIndexOf(2n ** 64n)].join()`, "true,true,true,0,2,a,true,false,false,0"},
		{"literal past the size limit", `const s = "9".repeat(400000); return [1n < s, -1n > "-" + s, 1n == s, s > 2n ** 100n].join()`, "true,true,false,true"},
	})
}

// A BigInt literal is held to maxBigIntBits like the result of every
// operation: the largest one the lexer accepts is the largest value, and it
// compares equal to its own decimal text.
func TestBigIntLiteralSizeLimit(t *testing.T) {
	largest := "0x" + strings.Repeat("f", maxBigIntBits/4) + "n"
	runProtoCases(t, []protoCase{
		{"largest", `const x = ` + largest + `, s = x.toString(); return [x.toString(2).length, x == s, s == x, x < s + "0", x > "9".repeat(400000), x === (2n ** 1048575n - 1n) * 2n + 1n].join()`, "1048576,true,true,true,false,true"},
		{"past the largest", `return ` + largest + ` + 1n`, "!RangeError: Maximum BigInt size exceeded"},
	})
	for _, lit := range []string{"0x1" + strings.Repeat("0", maxBigIntBits/4) + "n", "1" + strings.Repeat("0", 400000) + "n"} {
		_, err := syntax.ParseModule("t.js", "export const x = "+lit, syntax.Options{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Maximum BigInt size exceeded")
	}
}

// A 0x, 0o or 0b BigInt literal too large for 64 bits reaches the engine
// as hex, not decimal text, which math/big converts in superlinear time
// (about 50 ms and 200 MB to parse the largest one): its value and its
// property key are those of its digits, and compiling and running the
// largest one costs about its size.
func TestBigIntRadixLiteral(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"values", `return [0xffffffffffffffffn, 0x1ffffffffffffffffn, 0o7777777777777777777777n, 0b1_0000000000000000000000000000000000000000000000000000000000000000n, 0x0000_1_0000_0000_0000_0000n].join()`, "18446744073709551615,36893488147419103231,73786976294838206463,18446744073709551616,18446744073709551616"},
		{"equal to decimal", `return [0x1ffffffffffffffffn === 36893488147419103231n, -0x1ffffffffffffffffn === -36893488147419103231n, typeof 0o7777777777777777777777n].join()`, "true,true,bigint"},
		{"keys", `const o = { 0x1ffffffffffffffffn: 1, 0b1_0000000000000000000000000000000000000000000000000000000000000000n() { return 2 } }; class C { static 0o7777777777777777777777n = 3 } return [...Object.keys(o), ...Object.keys(C), o["36893488147419103231"], o["18446744073709551616"](), C["73786976294838206463"]].join()`, "36893488147419103231,18446744073709551616,73786976294838206463,1,2,3"},
	})
	largest := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), maxBigIntBits), big.NewInt(1))
	for _, base := range []int{16, 8, 2} {
		prefix := map[int]string{16: "0x", 8: "0o", 2: "0b"}[base]
		src := "export const x = " + prefix + largest.Text(base) + "n"
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f := evalModule(t, src)
		runtime.ReadMemStats(&after)
		v, ok := f.env.GetBindingValue("x")
		require.True(t, ok)
		assert.True(t, v.IsBigInt() && v.AsBigInt().v.Cmp(largest) == 0, prefix)
		assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(24*len(src)), prefix)
	}
}

func TestBigIntBuiltins(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"conversions", `return [typeof 1n, typeof Object(1n), !!0n, !!1n, 0n ? 1 : 2, String(-0n), ` + "`${10n ** 20n}`" + `, Number(1n), Number(2n ** 64n), Number(-(2n ** 53n) - 1n), Number(2n ** 1024n), Number(-(2n ** 1024n))].join()`, "bigint,object,false,true,2,0,100000000000000000000,1,18446744073709552000,-9007199254740992,Infinity,-Infinity"},
		{"property keys", `const o = {}; o[1n] = "a"; o[2n ** 64n] = "b"; return [o[1], o["18446744073709551616"], Object.keys(o).join("|"), [10, 20][1n], 1n in [0, 1]].join()`, "a,b,1|18446744073709551616,20,true"},
		{"BigInt()", `return [BigInt(10), BigInt("0x1f"), BigInt(" 12 "), BigInt(true), BigInt(""), BigInt("-7"), BigInt(Number.MAX_SAFE_INTEGER) + 2n, BigInt(1e21), BigInt(-0), BigInt("0b101"), BigInt("0o17"), BigInt({valueOf() { return 3 }}), BigInt.length, BigInt.name, typeof BigInt].join()`, "10,31,12,1,0,-7,9007199254740993,1000000000000000000000,0,5,15,3,1,BigInt,function"},
		{"BigInt(1.5)", `return BigInt(1.5)`, "!RangeError: The number 1.5 cannot be converted to a BigInt because it is not an integer"},
		{"BigInt(NaN)", `return BigInt(NaN)`, "!RangeError: The number NaN cannot be converted to a BigInt because it is not an integer"},
		{"BigInt(Infinity)", `return BigInt(-Infinity)`, "!RangeError: The number -Infinity cannot be converted to a BigInt because it is not an integer"},
		{"BigInt decimal string", `return BigInt("1.5")`, "!SyntaxError: Cannot convert 1.5 to a BigInt"},
		{"BigInt empty prefix", `return BigInt("0x")`, "!SyntaxError: Cannot convert 0x to a BigInt"},
		{"BigInt separator", `return BigInt("1_000")`, "!SyntaxError"},
		{"BigInt prefixed separator", `return BigInt("0x1_0")`, "!SyntaxError"},
		{"BigInt signed prefix", `return BigInt("-0x1")`, "!SyntaxError"},
		{"BigInt suffix", `return BigInt("1n")`, "!SyntaxError"},
		{"BigInt(undefined)", `return BigInt(undefined)`, "!TypeError: Cannot convert undefined to a BigInt"},
		{"BigInt(null)", `return BigInt(null)`, "!TypeError: Cannot convert null to a BigInt"},
		{"BigInt(symbol)", `return BigInt(Symbol())`, "!TypeError"},
		{"BigInt too large", `return BigInt("1".repeat(400000))`, "!RangeError: Maximum BigInt size exceeded"},
		{"new BigInt", `return new BigInt(1)`, "!TypeError: BigInt is not a constructor"},
		{"extends BigInt", `class B extends BigInt {} return new B(1)`, "!TypeError: BigInt is not a constructor"},
		{"asIntN", `return [BigInt.asIntN(8, 255n), BigInt.asIntN(8, 127n), BigInt.asIntN(8, 128n), BigInt.asIntN(8, -129n), BigInt.asIntN(0, 5n), BigInt.asIntN(64, 2n ** 63n), BigInt.asIntN(2 ** 53 - 1, -5n), BigInt.asIntN(1, 1n), BigInt.asIntN(1, -1n), BigInt.asIntN(8, -128n), BigInt.asIntN(200, 2n ** 199n), BigInt.asIntN.length].join()`, "-1,127,-128,127,0,-9223372036854775808,-5,-1,-1,-128,-803469022129495137770981046170581301261101496891396417650688,2"},
		{"asUintN", `return [BigInt.asUintN(8, -1n), BigInt.asUintN(8, 257n), BigInt.asUintN(0, 5n), BigInt.asUintN(64, -1n), BigInt.asUintN(2 ** 53 - 1, 5n), BigInt.asUintN(8, 255n), BigInt.asUintN(3, -9n), BigInt.asUintN("8", true)].join()`, "255,1,0,18446744073709551615,5,255,7,1"},
		{"asUintN too large", `return BigInt.asUintN(2 ** 53 - 1, -1n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"asIntN bits", `return BigInt.asIntN(-1, 1n)`, "!RangeError"},
		{"asIntN number", `return BigInt.asIntN(8, 1)`, "!TypeError: Cannot convert 1 to a BigInt"},
		{"asIntN order", `const log = []; BigInt.asIntN({valueOf() { log.push("bits"); return 8 }}, {valueOf() { log.push("bigint"); return 1n }}); return log.join()`, "bits,bigint"},
		{"prototype", `return [(255n).toString(16), (-255n).toString(2), (10n).toLocaleString(), (-(10n ** 21n)).toLocaleString(), BigInt.prototype.toLocaleString.name, BigInt.prototype.constructor === BigInt, Object.getPrototypeOf(1n) === BigInt.prototype, BigInt.prototype[Symbol.toStringTag]].join()`, "ff,-11111111,10,-1,000,000,000,000,000,000,000,toLocaleString,true,true,BigInt"},
		{"prototype without the global", `return [(12n).toLocaleString(), (1n).constructor.name, Object(1n).constructor.name, Object.getPrototypeOf(Object(1n)).toLocaleString.name].join()`, "12,BigInt,BigInt,toLocaleString"},
		{"wrapper", `const o = Object(5n); return [typeof o, o + 1n, o.valueOf() === 5n, o.toString(2), BigInt.prototype.valueOf.call(o) === 5n, BigInt.prototype.toLocaleString.call(o), Object.prototype.toString.call(o), o == 5n, o === 5n, o instanceof BigInt].join()`, "object,6,true,101,true,5,[object BigInt],true,false,true"},
		{"valueOf receiver", `return BigInt.prototype.valueOf.call(1)`, "!TypeError: BigInt.prototype.valueOf requires that 'this' be a BigInt"},
		{"toString receiver", `return BigInt.prototype.toString.call({})`, "!TypeError: BigInt.prototype.toString requires that 'this' be a BigInt"},
		{"toLocaleString receiver", `return BigInt.prototype.toLocaleString.call("1")`, "!TypeError: BigInt.prototype.toLocaleString requires that 'this' be a BigInt"},
		{"JSON", `return JSON.stringify({a: 1n})`, "!TypeError: Do not know how to serialize a BigInt"},
		{"JSON wrapper", `return JSON.stringify([Object(1n)])`, "!TypeError: Do not know how to serialize a BigInt"},
		{"JSON replacer", `return JSON.stringify({a: 1n, b: [2n]}, (k, v) => typeof v === "bigint" ? String(v) : v)`, `{"a":"1","b":["2"]}`},
	})
}

// TestBigIntToJSON checks that a toJSON on the mutable BigInt.prototype
// serializes bigints.
func TestBigIntToJSON(t *testing.T) {
	assert.Equal(t, `{"a":"1n"}`, evalProtoBody(t, `BigInt.prototype.toJSON = function () { return this.toString() + "n" }; return JSON.stringify({a: 1n})`, false))
}

// TestBigIntLateGlobal checks that a mutable realm installs BigInt only
// when a program first reaches it, and that a bigint's prototype methods
// install it.
func TestBigIntLateGlobal(t *testing.T) {
	r := NewRealm()
	key := StringKey(AtomBigInt)
	_, _, ok := r.Global.shape.Lookup(key)
	assert.False(t, ok)
	_, err := r.GetV(BigIntValue(NewBigIntFromInt64(1)), StringKey(AtomToLocaleString))
	require.NoError(t, err)
	v, ok := r.Global.GetOwnDataValue(key)
	require.True(t, ok)
	assert.True(t, IsConstructor(v))
}

// TestBigIntConversionInterrupt checks that converting a large BigInt to a
// string, or parsing a long string as one, stops at a pending interrupt
// (such a conversion takes up to tens of milliseconds, and a native loop
// checks the interrupt only every interruptStride callbacks), while a small
// conversion does not look.
func TestBigIntConversionInterrupt(t *testing.T) {
	r := NewRealm()
	x, ok := NewBigIntFromBig(new(big.Int).Lsh(big.NewInt(3), maxBigIntBits-2))
	require.True(t, ok)
	large, small := BigIntValue(x), BigIntValue(NewBigIntFromInt64(-12345))
	long := StringValue(FromGoString(strings.Repeat("7", bigintSlowChars+1)))
	short := StringValue(FromGoString(strings.Repeat("7", bigintSlowChars)))
	convs := []struct {
		name string
		conv func(big, str Value) error
	}{
		{"ToString", func(b, _ Value) error { _, err := r.ToString(b); return err }},
		{"toString", func(b, _ Value) error { _, err := auditBMethod(t, r, b, "toString"); return err }},
		{"toString(16)", func(b, _ Value) error { _, err := auditBMethod(t, r, b, "toString", IntValue(16)); return err }},
		{"toLocaleString", func(b, _ Value) error { _, err := auditBMethod(t, r, b, "toLocaleString"); return err }},
		{"ToBigInt", func(_, s Value) error { _, err := r.ToBigInt(s); return err }},
		{"BigInt < string", func(_, s Value) error { _, err := r.IsLessThan(small, s, true); return err }},
		{"string < BigInt", func(_, s Value) error { _, err := r.IsLessThan(s, small, true); return err }},
		{"BigInt == string", func(_, s Value) error { _, err := r.LooseEquals(small, s); return err }},
		{"string == BigInt", func(_, s Value) error { _, err := r.LooseEquals(s, small); return err }},
	}
	r.Interrupt("stop")
	defer r.ClearInterrupt()
	for _, c := range convs {
		var ie *InterruptedError
		assert.True(t, errors.As(c.conv(large, long), &ie), "%s of a large value ran with an interrupt pending", c.name)
		assert.NoError(t, c.conv(small, short), "%s of a small value", c.name)
	}
}

func TestBigIntConversions(t *testing.T) {
	r := NewRealm()
	b := func(s string) Value {
		x, ok := NewBigIntFromDecimal(s)
		require.True(t, ok)
		return BigIntValue(x)
	}
	for _, c := range []struct {
		in  Value
		i64 int64
		u64 uint64
	}{
		{b("0"), 0, 0},
		{b("-1"), -1, math.MaxUint64},
		{b("18446744073709551615"), -1, math.MaxUint64},
		{b("18446744073709551616"), 0, 0},
		{b("9223372036854775808"), math.MinInt64, 1 << 63},
		{b("-9223372036854775809"), math.MaxInt64, 1<<63 - 1},
		{b("340282366920938463463374607431768211457"), 1, 1},
		{True(), 1, 1},
		{str("0xff"), 255, 255},
		{str("-2"), -2, math.MaxUint64 - 1},
	} {
		i, err := r.ToBigInt64(c.in)
		require.NoError(t, err)
		assert.Equal(t, c.i64, i, "ToBigInt64(%v)", c.in)
		u, err := r.ToBigUint64(c.in)
		require.NoError(t, err)
		assert.Equal(t, c.u64, u, "ToBigUint64(%v)", c.in)
	}
	for _, v := range []Value{IntValue(1), Undefined(), Null()} {
		_, err := r.ToBigInt(v)
		assert.ErrorContains(t, err, "TypeError: Cannot convert", "%v", v)
	}
	_, err := r.ToBigInt(str("1.5"))
	assert.EqualError(t, err, "SyntaxError: Cannot convert 1.5 to a BigInt")

	assert.Equal(t, "18446744073709551615", NewBigIntFromUint64(math.MaxUint64).ToString())
	assert.Equal(t, uint64(math.MaxUint64), NewBigIntFromUint64(math.MaxUint64).Uint64())
	assert.Equal(t, int64(math.MinInt64), NewBigIntFromInt64(math.MinInt64).Int64())
	x, ok := NewBigIntFromBig(new(big.Int).Lsh(big.NewInt(-3), 100))
	require.True(t, ok)
	assert.Equal(t, "-3802951800684688204490109616128", x.ToString())
	_, ok = NewBigIntFromBig(new(big.Int).Lsh(big.NewInt(1), maxBigIntBits))
	assert.False(t, ok)

	for s, want := range map[string]string{"": "0", " 0x1F\n": "31", "-0": "0", "+5": "5", "0B11": "3", "0O7": "7", "007": "7"} {
		x, ok := StringToBigInt(FromGoString(s))
		require.True(t, ok, "%q", s)
		assert.Equal(t, want, x.ToString(), "%q", s)
	}
	for _, s := range []string{"-", "+", "0x", "1e3", "1.0", "0x1_0", "1_0", "--1", "+0x1", "0xg", "0b2", "0o8", "Infinity", "١"} {
		_, ok := StringToBigInt(FromGoString(s))
		assert.False(t, ok, "%q", s)
	}
}

// TestBigIntErrorMessageBounded checks that an error message shows a BigInt
// of more than 100 64-bit digits as V8 does, "<a very large BigInt>": the
// message is built without an interrupt check, and a native loop such as
// arr.map(Promise.all, Promise) builds one per callback, so it must cost no
// long conversion. A BigInt of up to 6400 bits is shown in full.
func TestBigIntErrorMessageBounded(t *testing.T) {
	f := evalModule(t, `
const msg = f => { try { f(); return "no error"; } catch (e) { return e.message; } };
const messages = b => [
  msg(() => { for (const x of b); }),
  msg(() => { const [a] = b; }),
  msg(() => Object.fromEntries(b)),
  msg(() => { function F() {} F.prototype = b; return ({}) instanceof F; }),
  msg(() => new Map([b])),
  msg(() => [1].map(b)),
  msg(() => Object.create(b)),
  msg(() => "x" in b),
];
const huge = (1n << 1048575n) - 1n;
export const got = [huge, -huge, 1n << 6400n].map(messages);
export const edge = messages((1n << 6400n) - 1n);
export const rejections = [];
for (const k of ["all", "any", "race", "allSettled"]) Promise[k](huge).catch(e => rejections.push(e.message));
new Promise(Object.fromEntries.bind(null, huge)).catch(e => rejections.push(e.message));
(async () => { for await (const x of huge); })().catch(e => rejections.push(e.message));
new Array(100).fill(huge).map(Promise.all, Promise).forEach(p => p.catch(e => rejections.push(e.message)));
export function thrower() { throw 1n << 6400n; }
`)
	templates := []string{
		"%s is not iterable",
		"%s is not iterable",
		"%s is not iterable",
		"Function has non-object prototype '%s' in instanceof check",
		"Iterator value %s is not an entry object",
		"%s is not a function",
		"Object prototype may only be an Object or null: %s",
		"Cannot use 'in' operator to search for 'x' in %s",
	}
	for i, msgs := range f.export("got").([]any) {
		for j, m := range msgs.([]any) {
			assert.Equal(t, fmt.Sprintf(templates[j], "<a very large BigInt>"), m, "value %d, message %d", i, j)
		}
	}
	edge := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 6400), big.NewInt(1)).String() + "n"
	for j, m := range f.export("edge").([]any) {
		assert.Equal(t, fmt.Sprintf(templates[j], edge), m, "message %d", j)
	}
	rejections := f.export("rejections").([]any)
	assert.Len(t, rejections, 106)
	for _, m := range rejections {
		assert.Equal(t, "<a very large BigInt> is not iterable", m)
	}
	// A thrown BigInt is the host's to show: Error and Message show it all.
	_, err := f.callErr("thrower")
	var exc *Exception
	require.ErrorAs(t, err, &exc)
	thrown := new(big.Int).Lsh(big.NewInt(1), 6400).String()
	assert.Equal(t, thrown+"n", exc.Error())
	assert.Equal(t, thrown, exc.Message())
}

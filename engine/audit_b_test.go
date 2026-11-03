package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Audit B: strings, JSON, URI, Date, conversions.
// Tests that demonstrate a bug carry t.Skip("audit finding: ...") as their
// first line so the suite stays green; remove the Skip to reproduce.

// auditBReprPrelude renders a value deterministically: strings through
// JSON.stringify (lone surrogates and controls become \uXXXX, everything
// else stays raw), -0 kept, arrays element-wise, objects by own keys.
const auditBReprPrelude = `
function repr(v) {
  if (v === undefined) return "undefined";
  if (v === null) return "null";
  if (typeof v === "number") return (v === 0 && 1 / v < 0) ? "-0" : String(v);
  if (typeof v === "string") return JSON.stringify(v);
  if (typeof v === "boolean") return String(v);
  if (typeof v === "bigint") return String(v) + "n";
  if (Array.isArray(v)) return "[" + v.map(repr).join(",") + "]";
  if (typeof v === "function") return "function";
  if (typeof v === "object") return "{" + Object.keys(v).map(function (k) { return JSON.stringify(k) + ":" + repr(v[k]); }).join(",") + "}";
  return String(v);
}
`

// auditBRealm compiles and evaluates src as a module in a fresh realm.
func auditBRealm(t *testing.T, src string) (*Realm, *ModuleEnv) {
	t.Helper()
	m, err := syntax.ParseModule("audit.js", src, syntax.Options{})
	require.NoError(t, err, "parse: %s", src)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	return r, env
}

// auditBCall calls exported function name with no arguments.
func auditBCall(t *testing.T, r *Realm, env *ModuleEnv, name string) (Value, error) {
	t.Helper()
	fn, ok := env.GetBindingValue(name)
	require.True(t, ok, "export %q", name)
	return r.Call(fn, Undefined(), nil)
}

// auditBExpr evaluates expr and returns repr(expr) or "throws Name: message".
func auditBExpr(t *testing.T, expr string) string {
	t.Helper()
	src := auditBReprPrelude + `
export function f() {
  try { return repr((` + expr + `)); }
  catch (e) { return "throws " + (e && e.name) + ": " + (e && e.message); }
}`
	r, env := auditBRealm(t, src)
	v, err := auditBCall(t, r, env, "f")
	if err != nil {
		return "hosterr " + err.Error()
	}
	return v.AsString().GoString()
}

// auditBStmt evaluates a statement body that returns a string.
func auditBStmt(t *testing.T, body string) string {
	t.Helper()
	src := auditBReprPrelude + `
export function f() {
  try { ` + body + ` }
  catch (e) { return "throws " + (e && e.name) + ": " + (e && e.message); }
}`
	r, env := auditBRealm(t, src)
	v, err := auditBCall(t, r, env, "f")
	if err != nil {
		return "hosterr " + err.Error()
	}
	if !v.IsString() {
		return v.String()
	}
	return v.AsString().GoString()
}

// auditBRun evaluates a body and returns the raw Value/error (no repr).
func auditBRun(t *testing.T, body string) (*Realm, Value, error) {
	t.Helper()
	r, env := auditBRealm(t, `export function f() { `+body+` }`)
	v, err := auditBCall(t, r, env, "f")
	return r, v, err
}

// auditBRunPanics runs body and recovers a Go panic (a panic would kill the
// host process in production).
func auditBRunPanics(t *testing.T, body string) (rec any, v Value, err error) {
	t.Helper()
	r, env := auditBRealm(t, `export function f() { `+body+` }`)
	func() {
		defer func() { rec = recover() }()
		v, err = auditBCall(t, r, env, "f")
	}()
	return rec, v, err
}

func auditBErrName(err error) string {
	var exc *Exception
	if errors.As(err, &exc) {
		return errorDisplayString(exc.Value)
	}
	var ie *InterruptedError
	if errors.As(err, &ie) {
		return "Interrupted"
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// auditBMethod calls recv[name](args...) through the native directly (no
// interpreter frame, so the function-entry interrupt check is bypassed).
func auditBMethod(t *testing.T, r *Realm, recv Value, name string, args ...Value) (Value, error) {
	t.Helper()
	fn, err := r.GetV(recv, r.KeyFromGoString(name))
	require.NoError(t, err)
	require.True(t, IsCallable(fn), name)
	return r.Call(fn, recv, args)
}

func auditBTable(t *testing.T, cases [][2]string) {
	t.Helper()
	for _, c := range cases {
		t.Run(c[0], func(t *testing.T) {
			got := auditBExpr(t, c[0])
			assert.Equal(t, c[1], got, "expr: %s", c[0])
		})
	}
}

// =============================================================================================
// FINDINGS (each wrapped in t.Skip; remove the Skip to reproduce)
// =============================================================================================

// Finding 1 (HIGH, string.go Concat): `+` never enforces MaxStringLength and
// stores the rope length in an int32. 31 doublings of "x" produce a rope of
// 2^31 units with a negative .length; flattening a 2^32 rope panics with
// "makeslice: cap out of range" (a runtime.Error the facade re-panics, so
// the host process dies); a rope between MaxStringLength and 2^31 is
// accepted and allocates > 1 GiB on flatten.
func TestAuditB_ConcatRopeOverflowsInt32Length(t *testing.T) {
	rec, v, err := auditBRunPanics(t, `let s = 'x'; for (let i = 0; i < 31; i++) s += s; return s.length;`)
	require.Nil(t, rec, "panic: %v", rec)
	require.Error(t, err, "expected RangeError: Invalid string length, got value %v", v)
	assert.Contains(t, auditBErrName(err), "RangeError")
}

func TestAuditB_ConcatRopeFlattenPanics(t *testing.T) {
	rec, _, err := auditBRunPanics(t, `let s = 'x'; for (let i = 0; i < 32; i++) s += s; return s.charCodeAt(5);`)
	require.Nil(t, rec, "panic: %v", rec)
	require.Error(t, err)
	assert.Contains(t, auditBErrName(err), "RangeError")
}

// Concat checks the length arithmetic whatever the operands' representation,
// so the operands are ropes built by doubling: they reach the limit without
// allocating 512 MiB flat strings (20 s under -race).
func TestAuditB_ConcatJustAboveMaxAccepted(t *testing.T) {
	const rope = `function rope(n) { let r = "", p = "x"; for (; n > 0; n >>= 1) { if (n & 1) r = r + p; if (n > 1) p = p + p; } return r; } `
	_, v, err := auditBRun(t, rope+`let s = rope(1 << 29); let u = s + s; return u.length;`)
	require.Error(t, err, "expected RangeError, got length %v", v)
	assert.Contains(t, auditBErrName(err), "RangeError")
	_, v, err = auditBRun(t, rope+fmt.Sprintf(`return (rope(1 << 29) + rope(%d)).length;`, MaxStringLength-1<<29))
	require.NoError(t, err)
	assert.Equal(t, float64(MaxStringLength), v.AsNumber(), "a result of exactly MaxStringLength is accepted")
	_, v, err = auditBRun(t, rope+fmt.Sprintf(`return (rope(1 << 29) + rope(%d)).length;`, MaxStringLength-1<<29+1))
	require.Error(t, err, "expected RangeError, got length %v", v)
	assert.Contains(t, auditBErrName(err), "RangeError")
}

// Finding 2 (MEDIUM, builtin_uri.go:200): a valid UTF-8 encoding of U+FFFD
// (%EF%BF%BD) is rejected because the decoder conflates utf8.RuneError with a
// decode failure. encodeURIComponent("\uFFFD") does not round-trip.
func TestAuditB_DecodeURIReplacementCharacter(t *testing.T) {
	assert.Equal(t, `"`+"\uFFFD"+`"`, auditBExpr(t, `decodeURIComponent("%EF%BF%BD")`))
	assert.Equal(t, `"`+"\uFFFD"+`"`, auditBExpr(t, `decodeURI("%EF%BF%BD")`))
	assert.Equal(t, "true", auditBExpr(t, `decodeURIComponent(encodeURIComponent("\ufffd")) === "\ufffd"`))
	// Invalid sequences stay URIErrors and valid ones decode.
	auditBTable(t, [][2]string{
		{`decodeURIComponent("%ED%A0%80")`, "throws URIError: URI malformed"},    // surrogate
		{`decodeURIComponent("%C0%80")`, "throws URIError: URI malformed"},       // overlong
		{`decodeURIComponent("%F4%90%80%80")`, "throws URIError: URI malformed"}, // above U+10FFFF
		{`decodeURIComponent("%F0%9F%98%80")`, `"` + "\U0001f600" + `"`},
		{`decodeURIComponent("%E2%82%AC%25")`, `"€%"`},
	})
}

// Finding 3 (MEDIUM, builtin_json.go:490-497): JSON.parse scans every earlier
// key of the current object for each new key (duplicate detection), O(n^2)
// in the key count of one object. 20k keys ~120 ms, 80k keys ~1.8 s.
func TestAuditB_JSONParseDuplicateKeyScanIsQuadratic(t *testing.T) {
	// Duplicates keep their first position and take the last value on both
	// sides of the indexing threshold, and nested objects do not share the
	// index of the enclosing one.
	auditBTable(t, [][2]string{
		{`(() => { const o = JSON.parse('{"a":1,"b":2,"a":3}'); return [Object.keys(o), o.a]; })()`, `[["a","b"],3]`},
		{`(() => { let s = "{"; for (let i = 0; i < 200; i++) s += '"k' + i + '":' + i + ','; s += '"k5":"last","k199":"end","k0":{"k0":"inner","k1":1,"k0":"inner2"}}'; const o = JSON.parse(s); return [Object.keys(o).length, Object.keys(o)[5], o.k5, o.k199, o.k0.k0, Object.keys(o.k0), o.k1]; })()`, `[200,"k5","last","end","inner2",["k0","k1"],1]`},
		{`(() => { let s = "{"; for (let i = 0; i < 100; i++) s += '"k' + (i % 70) + '":' + i + ','; s += '"z":0}'; const o = JSON.parse(s); return [Object.keys(o).length, o.k0, o.k29, o.k30, o.k69]; })()`, `[71,70,99,30,69]`},
	})
	timeParse := func(n int) time.Duration {
		r, env := auditBRealm(t, `
export function mk(n) { let s = "{"; for (let i = 0; i < n; i++) s += (i ? "," : "") + '"k' + i + '":1'; return s + "}"; }`)
		fn, _ := env.GetBindingValue("mk")
		src, err := r.Call(fn, Undefined(), []Value{IntValue(n)})
		require.NoError(t, err)
		start := time.Now()
		_, err = auditBMethod(t, r, ObjectValue(r.JSON), "parse", src)
		require.NoError(t, err)
		return time.Since(start)
	}
	t20 := timeParse(20000)
	t80 := timeParse(80000)
	ratio := float64(t80) / float64(t20)
	t.Logf("20k keys: %s, 80k keys: %s, ratio %.1f (linear ~4, quadratic ~16)", t20, t80, ratio)
	assert.Less(t, ratio, 8.0, "JSON.parse is quadratic in keys per object")
}

// Finding 4 (LOW, convert.go:22-30): ToPrimitive(hint default) becomes hint
// number for every object; Date.prototype[@@toPrimitive] (21.4.4.45) treats
// "default" as "string", so `date + ""` and `date == x` are wrong.
func TestAuditB_DateToPrimitiveDefaultHint(t *testing.T) {
	assert.NotEqual(t, `"0"`, auditBExpr(t, `new Date(0) + ""`))
	assert.Equal(t, "false", auditBExpr(t, `new Date(5) == 5`))
	assert.Equal(t, "false", auditBExpr(t, `new Date(0) == "0"`))
	auditBTable(t, [][2]string{
		{`new Date(0) + "" === new Date(0).toString()`, "true"},
		{`new Date(5) == new Date(5).toString()`, "true"},
		{`new Date(5) - 0`, "5"}, // hint number
		{`+new Date(5)`, "5"},    // hint number
		{`` + "`${new Date(0)}`" + ` === String(new Date(0))`, "true"},
		{`new Date(0) < new Date(1)`, "true"},                                 // relational: hint number
		{`({valueOf() { return 1 }, toString() { return "s" }}) + ""`, `"1"`}, // ordinary objects keep the number hint
	})
}

// Finding 5 (LOW, fail-loudly): with no BigInt.prototype, method lookups on a
// BigInt primitive land on Object.prototype and return wrong values silently.
func TestAuditB_BigIntPrototypeMethodsSilentlyWrong(t *testing.T) {
	assert.Equal(t, `"10"`, auditBExpr(t, `(10n).toString()`))
	assert.Equal(t, `"bigint"`, auditBExpr(t, `typeof (10n).valueOf()`))
	auditBTable(t, [][2]string{
		{`(255n).toString(16)`, `"ff"`},
		{`(35n).toString(36) + (0n).toString(2)`, `"z0"`},
		{`(10n).toString(undefined)`, `"10"`},
		{`(123456789012345678901234567890n).toString(36)`, `"byw97um9s91dlz68tsi"`},
		{`(10n).toString(1)`, "throws RangeError: toString() radix must be between 2 and 36"},
		{`(10n).toString(37)`, "throws RangeError: toString() radix must be between 2 and 36"},
		{`(10n).valueOf() === 10n`, "true"},
		{`(10n).hasOwnProperty("x")`, "false"},
		{`String(10n) + (10n).toString()`, `"1010"`},
		{`(10n).toFixed`, "undefined"},
	})
}

// Finding 6 (LOW, fail-loudly): Date.prototype.toString is absent (documented),
// so String(date) resolves to Object.prototype.toString and returns
// "[object Date]" silently instead of the date text (dateCall already
// implements the format) or a TypeError.
func TestAuditB_DateToStringSilentlyWrong(t *testing.T) {
	assert.NotEqual(t, `"[object Date]"`, auditBExpr(t, `String(new Date(0))`))
	auditBTable(t, [][2]string{
		{`/^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d\d \d{4} \d\d:\d\d:\d\d GMT[+-]\d{4} \(.+\)$/.test(new Date(0).toString())`, "true"},
		{`new Date(0).toString() === String(new Date(0))`, "true"},
		{`new Date(NaN).toString()`, `"Invalid Date"`},
		{`String(new Date("x"))`, `"Invalid Date"`},
		{`new Date(86400000).toString().slice(4, 15).length`, "11"},
		{`Date.prototype.toString.call(new Date(0)).length > 20`, "true"},
		{`Object.prototype.toString.call(new Date(0))`, `"[object Date]"`},
		{`(() => { try { return Date.prototype.toString.call({}) } catch (e) { return e.name } })()`, `"TypeError"`},
	})
}

// Finding 7 (LOW, interrupt checks): natives that run for > 10 ms on inputs
// bounded only by MaxStringLength do not call CheckInterrupt:
// JSON.stringify's quote() of one long string, split by a string separator,
// padStart/padEnd's fill loop, UTF-16 toUpperCase/toLowerCase.
func TestAuditB_NativesWithoutInterruptChecks(t *testing.T) {
	str := func(s string) Value { return StringValue(FromGoString(s)) }
	for _, c := range []struct {
		name, mk string
		call     func(r *Realm, big Value) (Value, error)
	}{
		{"JSON.stringify 20MB string", `function mk(){ return "a\n".repeat(1e7); }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, ObjectValue(r.JSON), "stringify", big)
			}},
		{"JSON.stringify 1e6 utf16 string", `function mk(){ return "\u00e9\n".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, ObjectValue(r.JSON), "stringify", big)
			}},
		{"JSON.stringify long key", `function mk(){ const o = {}; o["k".repeat(1e6)] = 1; return o; }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, ObjectValue(r.JSON), "stringify", big)
			}},
		{"split 1e7", `function mk(){ return "a".repeat(1e7); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "split", str("a")) }},
		{"split 1e6 utf16", `function mk(){ return "\u00e9,".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "split", str(",")) }},
		{"padStart 1e7", `function mk(){ return "a"; }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, big, "padStart", IntValue(1e7), str("xy"))
			}},
		{"padEnd 1e7", `function mk(){ return "a"; }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, big, "padEnd", IntValue(1e7), str("x"))
			}},
		{"toUpperCase 1e7 utf16", `function mk(){ return "\u00e9".repeat(1e7); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "toUpperCase") }},
		{"toLowerCase 1e7 utf16", `function mk(){ return "\u00c9".repeat(1e7); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "toLowerCase") }},
	} {
		interrupted, elapsed, _ := auditBNativeInterrupt(t, c.mk, c.call)
		assert.True(t, interrupted, "%s ran %s with an interrupt pending", c.name, elapsed)
	}
}

// =============================================================================================
// CHECKED, NO FINDING
// =============================================================================================

// --- G: resource limits ------------------------------------------------------------------------

func TestAuditB_RepeatAndPadLimits(t *testing.T) {
	cases := [][2]string{
		{`"x".repeat(2**30)`, "throws RangeError: Invalid string length"},
		{`"x".repeat(2**31)`, "throws RangeError: Invalid string length"},
		{`"ab".repeat(1e9)`, "throws RangeError: Invalid string length"},
		{`"x".repeat(-1)`, "throws RangeError: Invalid count value: -1"},
		{`"x".repeat(Infinity)`, "throws RangeError: Invalid count value: Infinity"},
		{`"".repeat(2**40)`, `""`},
		{`"a".repeat(3.9)`, `"aaa"`},
		{`"a".repeat("2")`, `"aa"`},
		{`"x".padStart(2**31)`, "throws RangeError: Invalid string length"},
		{`"x".padEnd(1e10, "ab")`, "throws RangeError: Invalid string length"},
		{`"x".padStart(1e10, "")`, `"x"`},
		{`"x".split("", 1e9).length`, "1"},
		{`"abc".split("", 0).length`, "0"},
		{`String.fromCharCode.apply(null, new Array(100000).fill(65)).length`, "100000"},
	}
	for _, c := range cases {
		t.Run(c[0], func(t *testing.T) {
			start := time.Now()
			assert.Equal(t, c[1], auditBExpr(t, c[0]))
			assert.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

func TestAuditB_JSONDepthLimits(t *testing.T) {
	got := auditBStmt(t, `const s = "[".repeat(10001) + "]".repeat(10001); JSON.parse(s); return "ok";`)
	assert.Equal(t, "throws RangeError: JSON nesting is too deep (more than 10000 levels)", got)
	got = auditBStmt(t, `const s = "[".repeat(10000) + "]".repeat(10000); return String(Array.isArray(JSON.parse(s)));`)
	assert.Equal(t, "true", got)
	got = auditBStmt(t, `let v = []; let cur = v; for (let i = 0; i < 10000; i++) { const n = []; cur.push(n); cur = n; } JSON.stringify(v); return "ok";`)
	assert.Equal(t, "throws RangeError: JSON nesting is too deep (more than 10000 levels)", got)
	got = auditBStmt(t, `let v = []; let cur = v; for (let i = 0; i < 9999; i++) { const n = []; cur.push(n); cur = n; } return String(JSON.stringify(v).length);`)
	assert.Equal(t, "20000", got)
}

// auditBNativeInterrupt builds the input in JS, sets the interrupt, then calls
// the native from Go and reports whether it returned *InterruptedError.
func auditBNativeInterrupt(t *testing.T, setup string, call func(r *Realm, big Value) (Value, error)) (bool, time.Duration, error) {
	t.Helper()
	r, env := auditBRealm(t, setup+`
export const big = mk();`)
	bigV, ok := env.GetBindingValue("big")
	require.True(t, ok)
	r.Interrupt("x")
	start := time.Now()
	_, err := call(r, bigV)
	el := time.Since(start)
	var ie *InterruptedError
	return errors.As(err, &ie), el, err
}

// TestAuditB_NativeInterruptHonoured records which natives honour a pre-set
// interrupt and asserts it for the ones that must check (repeat,
// JSON.parse/stringify, join, fill, regexp). The inputs are far above the
// check intervals (4096 JSON values, 64 KiB of repeat output), so a native
// that stops checking still reports interrupted=false.
func TestAuditB_NativeInterruptHonoured(t *testing.T) {
	str := func(s string) Value { return StringValue(FromGoString(s)) }
	global := func(r *Realm, name string, arg Value) (Value, error) {
		fn, _ := r.Global.GetProp(r, r.KeyFromGoString(name))
		return r.Call(fn, Undefined(), []Value{arg})
	}
	cases := []struct {
		name, setup string
		call        func(r *Realm, big Value) (Value, error)
		required    bool
	}{
		{"JSON.parse 2MB array", `function mk(){ return "[" + "1,".repeat(1e6) + "1]"; }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, ObjectValue(r.JSON), "parse", big) }, true},
		{"JSON.parse 2MB single string", `function mk(){ return '"' + "a".repeat(2e6) + '"'; }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, ObjectValue(r.JSON), "parse", big) }, false},
		{"JSON.stringify 1e6 array", `function mk(){ return new Array(1e6).fill(1); }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, ObjectValue(r.JSON), "stringify", big)
			}, true},
		{"encodeURIComponent 1MB", `function mk(){ return "%".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) { return global(r, "encodeURIComponent", big) }, false},
		{"decodeURIComponent 900KB", `function mk(){ return "%41".repeat(3e5); }`,
			func(r *Realm, big Value) (Value, error) { return global(r, "decodeURIComponent", big) }, false},
		{"replaceAll 1e6", `function mk(){ return "a".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) {
				return auditBMethod(t, r, big, "replaceAll", str("a"), str("b"))
			}, false},
		{"indexOf 1e6", `function mk(){ return "a".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "indexOf", str("b")) }, false},
		{"trim 1e6", `function mk(){ return " ".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "trim") }, false},
		{"toUpperCase 1e6 ascii", `function mk(){ return "a".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "toUpperCase") }, false},
		{"repeat 1e6", `function mk(){ return "a"; }`,
			func(r *Realm, big Value) (Value, error) { return auditBMethod(t, r, big, "repeat", IntValue(1e6)) }, true},
		{"concat 1e6 x2 then flatten", `function mk(){ return "a".repeat(1e6); }`,
			func(r *Realm, big Value) (Value, error) {
				v, err := auditBMethod(t, r, big, "concat", big)
				if err != nil {
					return v, err
				}
				return auditBMethod(t, r, v, "charCodeAt", IntValue(3))
			}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interrupted, elapsed, err := auditBNativeInterrupt(t, c.setup, c.call)
			t.Logf("interrupted=%v elapsed=%s err=%v", interrupted, elapsed, auditBErrName(err))
			if c.required {
				assert.True(t, interrupted, "%s must honour a pending interrupt", c.name)
			}
			assert.Less(t, elapsed, 2*time.Second)
		})
	}
}

// --- D: spec conformance ------------------------------------------------------------------------

func TestAuditB_SpecStrings(t *testing.T) {
	auditBTable(t, [][2]string{
		{`"\uD83D".length`, "1"},
		{`"😀".length`, "2"},
		{`"😀".charCodeAt(0)`, "55357"},
		{`"😀".codePointAt(0)`, "128512"},
		{`"😀".codePointAt(1)`, "56832"},
		{`"😀".codePointAt(2)`, "undefined"},
		{`[..."😀a"].length`, "2"},
		{`(() => { let n = 0; for (const c of "😀a") n++; return n; })()`, "2"},
		{`"😀".slice(0,1) === "\uD83D"`, "true"},
		{`"😀".slice(0,1) + "😀".slice(1) === "😀"`, "true"},
		{`("😀".slice(0,1) + "😀".slice(1)).length`, "2"},
		{`"\uD800".toUpperCase() === "\uD800"`, "true"},
		{`"\uD800".toLowerCase() === "\uD800"`, "true"},
		{`"\uDC00\uD800".toUpperCase() === "\uDC00\uD800"`, "true"},
		{`"ß".toUpperCase()`, `"SS"`},
		{`"\u1e9e".toLowerCase()`, `"ß"`},
		{`"İ".toLowerCase().length`, "2"},
		{`"İ".toLowerCase() === "i\u0307"`, "true"},
		{`"i\u0307".toUpperCase() === "I\u0307"`, "true"},
		{`"Σ".toLowerCase()`, `"σ"`},
		{`"ΑΣ".toLowerCase()`, `"ας"`},
		{`"ΑΣ Σ".toLowerCase()`, `"ας σ"`},
		{`"\u03a3\u03a3".toLowerCase()`, `"σς"`},
		{`"A\u03a3'".toLowerCase()`, `"aς'"`},
		{`"A\u03a3\u0301b".toLowerCase() === "a\u03c3\u0301b"`, "true"},
		{`"ﬁ".toUpperCase()`, `"FI"`},
		{`"𐐀".toLowerCase() === "𐐨"`, "true"},
		{`"\ud801\udc28".toUpperCase() === "\ud801\udc00"`, "true"},
		{`"K".toLowerCase()`, `"k"`},
		{`"a".localeCompare("b")`, "-1"},
		{`"a".localeCompare("a")`, "0"},
		{`"b".localeCompare("a")`, "1"},
		{`"a".localeCompare()`, "-1"},
		{`"abc".at(-1)`, `"c"`},
		{`"abc".at(3)`, "undefined"},
		{`"a".at()`, `"a"`},
		{`"abcdef".substr(-3, 2)`, `"de"`},
		{`"abc".substr(-10, 2)`, `"ab"`},
		{`"abc".substr(1, -1)`, `""`},
		{`"abcdef".substring(3, 1)`, `"bc"`},
		{`"abc".substring(NaN, Infinity)`, `"abc"`},
		{`"abcdef".slice(3, 1)`, `""`},
		{`"abc".slice("1", "x")`, `""`},
		{`"abc".indexOf("", 5)`, "3"},
		{`"abc".indexOf("", Infinity)`, "3"},
		{`"abc".indexOf("c", -5)`, "2"},
		{`"abc".indexOf("c", Infinity)`, "-1"},
		{`"abc".indexOf("\u00e9")`, "-1"},
		{`"a\u00e9".indexOf("\u00e9")`, "1"},
		{`"abc".lastIndexOf("", 1)`, "1"},
		{`"abc".lastIndexOf("")`, "3"},
		{`"abc".lastIndexOf("c", -5)`, "-1"},
		{`"abcabc".lastIndexOf("c", NaN)`, "5"},
		{`"aa".lastIndexOf("a", 0.9)`, "0"},
		{`"abc".startsWith("", 100)`, "true"},
		{`"abc".endsWith("c", 2)`, "false"},
		{`"abc".endsWith("b", 2)`, "true"},
		{`"abc".includes("", 99)`, "true"},
		{`"abc".includes(/b/)`, "throws TypeError: First argument to String.prototype.includes must not be a regular expression"},
		{`"abc".split(undefined)`, `["abc"]`},
		{`"abc".split("", 0)`, `[]`},
		{`"abc".split("", 2)`, `["a","b"]`},
		{`"a,b,".split(",")`, `["a","b",""]`},
		{`"".split(",")`, `[""]`},
		{`"".split("")`, `[]`},
		{`"a,b,c".split(",", 2)`, `["a","b"]`},
		{`"a,b,c".split(",", -1)`, `["a","b","c"]`},
		{`"aXbXc".split("X", 4294967296)`, `[]`},
		{`"aXbXc".split("X", -4294967295)`, `["a"]`},
		{`"abc".split(",", "2")`, `["abc"]`},
		{`"éa,éb".split(",")`, `["éa","éb"]`},
		{`"éa,éb,".split(",", 2)`, `["éa","éb"]`},
		{`"\uFEFF x \n\u2028\u00A0\u3000".trim()`, `"x"`},
		{`"\u180E x".trim()`, `"` + "\u180e" + ` x"`}, // U+180E is not WhiteSpace since Unicode 6.3
		{`"\u200B x".trim()`, `"` + "\u200b" + ` x"`},
		{`"x".padStart(5, "")`, `"x"`},
		{`"x".padStart(5, "ab")`, `"ababx"`},
		{`"x".padEnd(3)`, `"x  "`},
		{`"x".padEnd(5, "ab")`, `"xabab"`},
		{`"abc".padStart(6, "😀")`, `"😀\ud83dabc"`}, // filler truncated by code units (22.1.3.17.1 StringPad)
		{`"abc".replaceAll("", "-")`, `"-a-b-c-"`},
		{`"abc".replace("", "-")`, `"-abc"`},
		{`"aaa".replaceAll("a", "$&$&")`, `"aaaaaa"`},
		{`"abc".replace("b", "$'")`, `"acc"`},
		{`"abc".replace("b", (m, p, s) => m + p + s)`, `"ab1abcc"`},
		{`"abc".replace("b", "$$")`, `"a$c"`},
		{`"abc".replace(/b/, "x", "extra")`, `"axc"`},
		{`"\u00e9\u00e9".replaceAll("\u00e9", "e")`, `"ee"`},
		{`"abc".concat(1, null)`, `"abc1null"`},
		{`"abc".charAt(-1)`, `""`},
		{`"abc".charCodeAt(3)`, "NaN"},
		{`"abc"[1]`, `"b"`},
		{`"abc"["length"]`, "3"},
		{`String.prototype.trim.call(null)`, "throws TypeError: String.prototype.trim called on null or undefined"},
		{`"a".codePointAt.call(undefined)`, "throws TypeError: String.prototype.codePointAt called on null or undefined"},
		{`String.prototype.slice.call(123, 1)`, `"23"`},
		{`String.fromCharCode(0x10000 + 65)`, `"A"`},
		{`String.fromCharCode(0xD83D, 0xDE00) === "😀"`, "true"},
		{`String.fromCharCode(-1) === "\uffff"`, "true"},
		{`String.fromCharCode(65.9)`, `"A"`},
		{`String.fromCharCode()`, `""`},
	})
}

func TestAuditB_SpecToStringToNumberEquality(t *testing.T) {
	auditBTable(t, [][2]string{
		{`String(-0)`, `"0"`},
		{`String(1e21)`, `"1e+21"`},
		{`String(1e-7)`, `"1e-7"`},
		{`String(123456789012345680000)`, `"123456789012345680000"`},
		{`String(0.1+0.2)`, `"0.30000000000000004"`},
		{`String(5e-324)`, `"5e-324"`},
		{`String(1.7976931348623157e308)`, `"1.7976931348623157e+308"`},
		{`String(-1e-7)`, `"-1e-7"`},
		{`String(0.000001)`, `"0.000001"`},
		{`String(1e20)`, `"100000000000000000000"`},
		{`String(2**53)`, `"9007199254740992"`},
		{`String(2**63)`, `"9223372036854776000"`},
		{`String(-1e15)`, `"-1000000000000000"`},
		{`String(1.5e-6)`, `"0.0000015"`},
		{`String(123e-20)`, `"1.23e-18"`},
		{`(25).toString(36)`, `"p"`},
		{`(255).toString(16)`, `"ff"`},
		{`(-255).toString(2)`, `"-11111111"`},
		{`(0.5).toString(2)`, `"0.1"`},
		{`String(null)`, `"null"`},
		{`String(undefined)`, `"undefined"`},
		{`String([1,[2,3]])`, `"1,2,3"`},
		{`String({})`, `"[object Object]"`},
		{`String([null, undefined])`, `","`},
		{`String(10n)`, `"10"`},
		{`1n + ""`, `"1"`},
		{`"" + {toString(){return 1}}`, `"1"`},
		{`"" + {valueOf(){return 2}, toString(){return "s"}}`, `"2"`}, // + uses hint default -> valueOf first (13.15.3 / 7.1.1)
		{`1 + {valueOf(){return 2}}`, "3"},
		{"`${{valueOf(){return 1}, toString(){return \"t\"}}}`", `"t"`}, // template literal: ToString -> hint string
		{`String({valueOf(){return 1}, toString(){return "t"}})`, `"t"`},
		{`"" + {valueOf(){return {}}, toString(){return "t"}}`, `"t"`},
		{`"" + {valueOf(){return {}}, toString(){return {}}}`, "throws TypeError: Cannot convert object to primitive value"},
		{`[1] == 1`, "true"},
		{`null == undefined`, "true"},
		{`null == 0`, "false"},
		{`null == false`, "false"},
		{`undefined == 0`, "false"},
		{`"" == 0`, "true"},
		{`"0" == false`, "true"},
		{`[] == ""`, "true"},
		{`[0] == false`, "true"},
		{`[] == 0`, "true"},
		{`[[]] == 0`, "true"},
		{`NaN == NaN`, "false"},
		{`({}) == "[object Object]"`, "true"},
		{`"1" == 1n`, "true"},
		{`1n == 1`, "true"},
		{`1n == 1.5`, "false"},
		{`1n === 1`, "false"},
		{`1n < 2`, "true"},
		{`2n > 1`, "true"},
		{`1n <= 1`, "true"},
		{`1n < "2"`, "true"},
		{`1n < "x"`, "false"},
		{`typeof null`, `"object"`},
		{`typeof function(){}`, `"function"`},
		{`typeof 1n`, `"bigint"`},
		{`typeof undefinedVar`, `"undefined"`},
		{`void 0 === undefined`, "true"},
		{`typeof Object.is`, `"function"`},
		{`Object.is(1, 1)`, "true"},
		{`+"0x10"`, "16"},
		{`+"0X1F"`, "31"},
		{`-"0b1"`, "-1"},
		{`+"1e"`, "NaN"},
		{`+" \n"`, "0"},
		{`+"\uFEFF 12 \u2028"`, "12"},
		{`+"1,2"`, "NaN"},
		{`+[]`, "0"},
		{`+[[]]`, "0"},
		{`+[1]`, "1"},
		{`+[1,2]`, "NaN"},
		{`+"Infinity"`, "Infinity"},
		{`+"-Infinity"`, "-Infinity"},
		{`+"+Infinity"`, "Infinity"},
		{`+"infinity"`, "NaN"},
		{`+"1__0"`, "NaN"},
		{`+"1_0"`, "NaN"},
		{`+"0o17"`, "15"},
		{`+"1e+"`, "NaN"},
		{`+"."`, "NaN"},
		{`+"-"`, "NaN"},
		{`+"+0"`, "0"},
		{`+"-0"`, "-0"},
		{`+"-0x10"`, "NaN"},
		{`+"0x"`, "NaN"},
		{`+"1."`, "1"},
		{`+".5"`, "0.5"},
		{`+"00.5"`, "0.5"},
		{`+"1e1000"`, "Infinity"},
		{`+"1e-400"`, "0"},
		{`+"0b2"`, "NaN"},
		{`+"0o8"`, "NaN"},
		{`+"0xg"`, "NaN"},
		{`+"12px"`, "NaN"},
		{`+"0x1fffffffffffff"`, "9007199254740991"},
		{`+"0x20000000000001"`, "9007199254740992"},
		{`+"0x20000000000003"`, "9007199254740996"},
		{`+null`, "0"},
		{`+undefined`, "NaN"},
		{`+true`, "1"},
		{`+{}`, "NaN"},
		{`+"١"`, "NaN"},
		{`Number("")`, "0"},
		{`Number(" ")`, "0"},
		{`Number("\u00A0")`, "0"},
	})
}

func TestAuditB_SpecJSON(t *testing.T) {
	auditBTable(t, [][2]string{
		{`JSON.stringify(-0)`, `"0"`},
		{`JSON.stringify([-0])`, `"[0]"`},
		{`JSON.stringify(1e21)`, `"1e+21"`},
		{`JSON.stringify(1e-7)`, `"1e-7"`},
		{`JSON.stringify(NaN)`, `"null"`},
		{`JSON.stringify(Infinity)`, `"null"`},
		{`JSON.stringify(undefined)`, "undefined"},
		{`JSON.stringify(function(){})`, "undefined"},
		{`JSON.stringify(null)`, `"null"`},
		{`JSON.stringify({a:undefined,b:function(){},c:1})`, `"{\"c\":1}"`},
		{`JSON.stringify([undefined, function(){}])`, `"[null,null]"`},
		{`JSON.stringify([,1])`, `"[null,1]"`},
		{`JSON.stringify({toJSON(){return {x:{toJSON(){return 5}}}}})`, `"{\"x\":5}"`},
		{`JSON.stringify({toJSON(k){return k}})`, `"\"\""`},
		{`JSON.stringify({a:{toJSON(k){return k}}})`, `"{\"a\":\"a\"}"`},
		{`JSON.stringify([{toJSON(k){return k}}])`, `"[\"0\"]"`},
		{`JSON.stringify(new Date(0))`, `"\"1970-01-01T00:00:00.000Z\""`},
		{`JSON.stringify(new Date(NaN))`, `"null"`},
		{`JSON.stringify({a:new Date(NaN)})`, `"{\"a\":null}"`},
		{`JSON.stringify(/a/g)`, `"{}"`},
		{`JSON.stringify(new Error("x"))`, `"{}"`},
		{`JSON.stringify(Object.freeze({a:1}))`, `"{\"a\":1}"`},
		{`JSON.stringify({a:1,b:2,c:3}, ["b","a", 1, "b"])`, `"{\"b\":2,\"a\":1}"`},
		{`JSON.stringify({1:1, a:2}, [1])`, `"{\"1\":1}"`},
		{`JSON.stringify({a:{b:1,c:2}}, ["a","b"])`, `"{\"a\":{\"b\":1}}"`},
		{`JSON.stringify([{a:1,b:2}], ["a"])`, `"[{\"a\":1}]"`},
		{`JSON.stringify({a:1}, [], 2)`, `"{}"`},
		{`JSON.stringify({a:1}, [new String("a")])`, `"{\"a\":1}"`},
		{`JSON.stringify({a:1,b:2}, [true, "b"])`, `"{\"b\":2}"`},
		{`JSON.stringify([1], ["0"])`, `"[1]"`},
		{`JSON.stringify({a:1}, null, 2)`, `"{\n  \"a\": 1\n}"`},
		{`JSON.stringify({a:1}, null, "abcdefghijkl")`, `"{\nabcdefghij\"a\": 1\n}"`},
		{`JSON.stringify({a:1}, null, 100)`, `"{\n          \"a\": 1\n}"`},
		{`JSON.stringify({a:1}, null, 0.5)`, `"{\"a\":1}"`},
		{`JSON.stringify({a:1}, null, new Number(2))`, `"{\n  \"a\": 1\n}"`},
		{`JSON.stringify({a:1}, null, new String("\t"))`, `"{\n\t\"a\": 1\n}"`},
		{`JSON.stringify({a:[]}, null, 2)`, `"{\n  \"a\": []\n}"`},
		{`JSON.stringify({a:{}}, null, 2)`, `"{\n  \"a\": {}\n}"`},
		{`JSON.stringify([1,[2]], null, 1)`, `"[\n 1,\n [\n  2\n ]\n]"`},
		{`JSON.stringify({a:1,b:[1]}, ["a","b"], 2)`, `"{\n  \"a\": 1,\n  \"b\": [\n    1\n  ]\n}"`},
		{`JSON.stringify({}, null, 2)`, `"{}"`},
		{`JSON.stringify([], null, 2)`, `"[]"`},
		{`JSON.stringify([[]], null, 2)`, `"[\n  []\n]"`},
		{`JSON.stringify("x", null, 2)`, `"\"x\""`},
		{`JSON.stringify("\u2028\ud800\u0007")`, `"\"` + "\u2028" + `\\ud800\\u0007\""`}, // U+2028 raw, lone surrogate and control escaped (25.5.2.3)
		{`JSON.stringify("\udbff\udfff")`, `"\"` + "\U0010FFFF" + `\""`},
		{`JSON.stringify("\udc00\ud800")`, `"\"\\udc00\\ud800\""`},
		{`JSON.stringify("\b\f\n\r\t\"\\")`, `"\"\\b\\f\\n\\r\\t\\\"\\\\\""`},
		{`JSON.stringify("\u001f\u007f")`, `"\"\\u001f` + "\x7f" + `\""`},
		{`JSON.stringify("\u007f\u0080\u00ff")`, `"\"` + "\x7f\u0080\u00ff" + `\""`},
		{`JSON.stringify({"\ud800": 1})`, `"{\"\\ud800\":1}"`},
		{`JSON.stringify({"é": 1})`, `"{\"é\":1}"`},
		{`JSON.stringify(Object.create(null))`, `"{}"`},
		{`JSON.stringify([1,2], (k,v)=> typeof v === 'number' ? v*2 : v)`, `"[2,4]"`},
		{`JSON.stringify(5, function(k,v){ return k === "" && this[""] === 5 ? "root" : v })`, `"\"root\""`},
		{`JSON.stringify({a:1}, function(k,v){ return k === "a" && this.a === 1 ? "hit" : v })`, `"{\"a\":\"hit\"}"`},
		{`JSON.stringify({a:1}, (k,v) => undefined)`, "undefined"},
		{`JSON.stringify({a:1,b:2}, (k,v) => k === "a" ? undefined : v)`, `"{\"b\":2}"`},
		{`JSON.stringify([1], (k,v) => k === "0" ? undefined : v)`, `"[null]"`},
		{`JSON.stringify({}, (k,v) => v, "  ")`, `"{}"`},
		{`JSON.stringify(new String("x"))`, `"\"x\""`},
		{`JSON.stringify(new Number(1))`, `"1"`},
		{`JSON.stringify(new Boolean(false))`, `"false"`},
		{`JSON.stringify([new String("x"), new Number(-0)])`, `"[\"x\",0]"`},
		{`JSON.stringify(1n)`, "throws TypeError: Do not know how to serialize a BigInt"},
		{`(() => { const o = {}; o.self = o; return JSON.stringify(o); })()`, "throws TypeError: Converting circular structure to JSON"},
		{`(() => { const a = [1]; a.push(a); return JSON.stringify(a); })()`, "throws TypeError: Converting circular structure to JSON"},
		{`(() => { const o = {}; return JSON.stringify([o, o]); })()`, `"[{},{}]"`},
		{`JSON.stringify({b:1, a:2, 1:3, 0:4})`, `"{\"0\":4,\"1\":3,\"b\":1,\"a\":2}"`},
		{`JSON.stringify(Object.defineProperty({a:1}, "h", {value: 2, enumerable: false}))`, `"{\"a\":1}"`},
		{`JSON.stringify({a:1, toJSON: undefined})`, `"{\"a\":1}"`},
		{`JSON.stringify({a: 1, b: {toJSON(){ return undefined }}})`, `"{\"a\":1}"`},
		{`JSON.stringify("a".repeat(70000)).length`, "70002"},
		{`JSON.parse("1e1000")`, "Infinity"},
		{`JSON.parse("-0")`, "-0"},
		{`JSON.parse("-0.0")`, "-0"},
		{`JSON.parse("0")`, "0"},
		{`JSON.parse('"\\ud800"').charCodeAt(0)`, "55296"},
		{`JSON.parse('"\\ud83d\\ude00"') === "😀"`, "true"},
		{`JSON.parse('"😀"').length`, "2"},
		{`JSON.parse('"\ud800"').charCodeAt(0)`, "55296"},
		{`JSON.parse('"a\ud800b"').length`, "3"},
		{`JSON.parse('{"\\ud800":1}')["\ud800"]`, "1"},
		{`Object.keys(JSON.parse('{"__proto__":1}'))`, `["__proto__"]`},
		{`Object.getPrototypeOf(JSON.parse('{"__proto__":1}')) === Object.prototype`, "true"},
		{`JSON.parse('{"__proto__":{"x":1}}').x`, "undefined"},
		{`JSON.parse('{"a":1,"a":2}').a`, "2"},
		{`Object.keys(JSON.parse('{"a":1,"b":1,"a":2}'))`, `["a","b"]`},
		{`JSON.parse(' [1] ')`, "[1]"},
		{`JSON.parse('\t\n\r [1]')`, "[1]"},
		{`JSON.parse('\u00A0[1]')`, "throws SyntaxError: Unexpected token \u00a0 in JSON at position 0"},
		{`JSON.parse("01")`, "throws SyntaxError: Unexpected token 1 in JSON at position 1"},
		{`JSON.parse("1.")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse(".1")`, "throws SyntaxError: Unexpected token . in JSON at position 0"},
		{`JSON.parse("+1")`, "throws SyntaxError: Unexpected token + in JSON at position 0"},
		{`JSON.parse("-")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("1e")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse('"\t"')`, "throws SyntaxError: Bad control character in string literal in JSON at position 1"},
		{`JSON.parse("[1,]")`, "throws SyntaxError: Unexpected token ] in JSON at position 3"},
		{`JSON.parse("{,}")`, "throws SyntaxError: Unexpected token , in JSON at position 1"},
		{`JSON.parse('{"a":1,}')`, "throws SyntaxError: Unexpected token } in JSON at position 7"},
		{`JSON.parse("[1 2]")`, "throws SyntaxError: Unexpected token 2 in JSON at position 3"},
		{`JSON.parse("tru")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("trux")`, "throws SyntaxError: Unexpected token x in JSON at position 3"},
		{`JSON.parse("''")`, "throws SyntaxError: Unexpected token ' in JSON at position 0"},
		{`JSON.parse('"\\x41"')`, "throws SyntaxError: Bad escaped character in JSON at position 2"},
		{`JSON.parse('"\\u12"')`, "throws SyntaxError: Unexpected end of JSON input"}, // V8: "Bad Unicode escape ... position 5" (message only)
		{`JSON.parse('"\\u12')`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("[")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("{")`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse('{"a"')`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse('{"a":')`, "throws SyntaxError: Unexpected end of JSON input"},
		{`JSON.parse("1 1")`, "throws SyntaxError: Unexpected token 1 in JSON at position 2"},
		{`JSON.parse("Infinity")`, "throws SyntaxError: Unexpected token I in JSON at position 0"},
		{`JSON.parse("NaN")`, "throws SyntaxError: Unexpected token N in JSON at position 0"},
		{`JSON.parse(undefined)`, "throws SyntaxError: Unexpected token u in JSON at position 0"},
		{`JSON.parse(null)`, "null"},
		{`JSON.parse(true)`, "true"},
		{`JSON.parse(12)`, "12"},
		{`JSON.parse("9007199254740993")`, "9007199254740992"},
		{`JSON.parse("9999999999999999")`, "10000000000000000"},
		{`JSON.parse("-9007199254740993")`, "-9007199254740992"},
		{`JSON.parse("12345678901234567890")`, "12345678901234567000"},
		{`JSON.parse("1E2")`, "100"},
		{`JSON.parse("1e-2")`, "0.01"},
		{`JSON.parse("0.1")`, "0.1"},
		{`JSON.parse("[0e0]")`, "[0]"},
		{`JSON.parse("[" + "1,".repeat(70000) + "1]").length`, "70001"},
		{`JSON.stringify(JSON.parse('{"a":{"b":[1,{"c":null}]}}'))`, `"{\"a\":{\"b\":[1,{\"c\":null}]}}"`},
		{`Object.keys(JSON.parse('{"2":1,"1":2,"a":3}'))`, `["1","2","a"]`},
		{`Object.keys(JSON.parse('{"a":1,"0":2}'))`, `["0","a"]`},
		{`Object.keys(JSON.parse('{"b":1,"4294967295":2,"a":3,"4294967294":4}'))`, `["4294967294","b","4294967295","a"]`},
		{`JSON.parse('{"a":1,"b":2}', (k,v)=> typeof v === "number" ? v+1 : v)`, `{"a":2,"b":3}`},
		{`JSON.parse('[1,2]', (k,v)=> k==="0" ? undefined : v).length`, "2"},
		{`0 in JSON.parse('[1,2]', (k,v)=> k==="0" ? undefined : v)`, "false"},
		{`JSON.parse('{"a":{"b":1}}', function(k,v){ return k === "b" && this.b === 1 ? "in" : v }).a.b`, `"in"`},
		{`JSON.parse('1', (k,v) => k === "" ? "root" : v)`, `"root"`},
		{`JSON.parse('{"a":1}', function(k, v){ if (k === "") return Object.keys(this).join("|"); return v; })`, `""`},
		{`JSON.parse('[1]', (k,v) => typeof k)`, `"string"`},
		{`JSON.parse('{"1":1,"a":2}', function(k,v){ if (k !== "") this.seen = (this.seen||"") + k; return v; }).seen`, `"1a"`},
		{`JSON.parse('{"a":1}', 5)`, `{"a":1}`}, // non-callable reviver ignored (25.5.1 step 3: IsCallable)
		{`JSON.parse('{"a":1}', {})`, `{"a":1}`},
	})
}

func TestAuditB_SpecJSON65Keys(t *testing.T) {
	got := auditBStmt(t, `
		let src = "{";
		const keys = [];
		for (let i = 0; i < 65; i++) { keys.push("k" + i); src += (i ? "," : "") + '"k' + i + '":' + i; }
		src += "}";
		const o = JSON.parse(src);
		if (Object.keys(o).join(",") !== keys.join(",")) return "key order: " + Object.keys(o).join(",");
		if (JSON.stringify(o) !== src) return "roundtrip: " + JSON.stringify(o);
		return "ok";`)
	assert.Equal(t, "ok", got)
}

func TestAuditB_SpecDate(t *testing.T) {
	local := time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local).UnixMilli()
	may2001 := time.Date(2001, 5, 1, 0, 0, 0, 0, time.Local).UnixMilli()
	auditBTable(t, [][2]string{
		{`new Date("2024-01-01").getTime()`, "1704067200000"},
		{`new Date("2024-01-01T00:00:00").getTime()`, NumberToGoString(float64(local))},
		{`new Date("2024-01-01T00:00").getTime()`, NumberToGoString(float64(local))},
		{`new Date("2024-01-01T00:00:00Z").getTime()`, "1704067200000"},
		{`new Date("2024-01-01T00:00:00.123+02:00").getTime()`, "1704060000123"},
		{`new Date("2024-01-01T00:00:00.123-02:00").getTime()`, "1704074400123"},
		{`new Date("2024-01").getTime()`, "1704067200000"},
		{`new Date("2024").getTime()`, "1704067200000"},
		{`new Date("+002024-01-01").getTime()`, "1704067200000"},
		{`new Date("2024-13-01").getTime()`, "NaN"},
		{`new Date("2024-02-30").getTime()`, "1709251200000"}, // V8: the day overflows into March
		{`new Date("2024-02-29").getTime()`, "1709164800000"},
		{`new Date("2023-02-29").getTime()`, "1677628800000"},
		{`new Date("2024-01-01T25:00:00Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T23:60:00Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T23:59:60Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T24:00:00Z").getTime()`, "1704153600000"},
		{`new Date("2024-01-01T24:00:01Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T24:00:00.001Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T00:00:00.1Z").getTime()`, "1704067200100"}, // 1-digit ms: spec grammar says sss, V8 accepts; matches V8
		{`new Date("2024-01-01T00:00:00.12345Z").getTime()`, "1704067200123"},
		{`new Date("2024-01-01T00:00:00.Z").getTime()`, "NaN"},
		{`new Date("2024-01-01T00:00:00+0200").getTime()`, "1704060000000"}, // V8 accepts +HHMM
		{`new Date("2024-01-01T00:00:00+24:00").getTime()`, "NaN"},
		{`new Date("2024-01-01t00:00:00z").getTime()`, "1704067200000"},
		{`new Date("2024-01-01 00:00:00Z").getTime()`, "1704067200000"},         // legacy grammar
		{`new Date(" 2024-01-01").getTime()`, NumberToGoString(float64(local))}, // legacy: local time
		{`new Date("2024-1-1").getTime()`, NumberToGoString(float64(local))},
		{`new Date("5").getTime()`, NumberToGoString(float64(may2001))}, // V8: month 5 of the default year 2001
		{`new Date("1e3").getTime()`, "NaN"},                            // string arg is parsed, not ToNumber'd
		{`new Date("").getTime()`, "NaN"},
		{`new Date(8.64e15).getTime()`, "8640000000000000"},
		{`new Date(8.64e15+1).getTime()`, "NaN"},
		{`new Date(-8.64e15-1).getTime()`, "NaN"},
		{`new Date(NaN).toISOString()`, "throws RangeError: Invalid time value"},
		{`new Date(0).toISOString()`, `"1970-01-01T00:00:00.000Z"`},
		{`new Date(-1).toISOString()`, `"1969-12-31T23:59:59.999Z"`},
		{`new Date(-62167219200000).toISOString()`, `"0000-01-01T00:00:00.000Z"`},
		{`new Date(-62167219200001).toISOString()`, `"-000001-12-31T23:59:59.999Z"`},
		{`new Date(-62198755200000).toISOString()`, `"-000001-01-01T00:00:00.000Z"`}, // (year -1, not 0: the brief's epoch was off by a year)
		{`new Date(253402300800000).toISOString()`, `"+010000-01-01T00:00:00.000Z"`},
		{`new Date("+010000-01-01T00:00:00.000Z").getTime()`, "253402300800000"},
		{`new Date("-000001-12-31T23:59:59.999Z").getTime()`, "-62167219200001"},
		{`new Date("-000000-01-01T00:00:00Z").getTime()`, "NaN"},
		{`new Date("+000000-01-01T00:00:00Z").getTime()`, "-62167219200000"},
		{`new Date(8.64e15).toISOString()`, `"+275760-09-13T00:00:00.000Z"`},
		{`new Date(-8.64e15).toISOString()`, `"-271821-04-20T00:00:00.000Z"`},
		{`new Date(1.9).getTime()`, "1"},
		{`new Date(-1.9).getTime()`, "-1"},
		{`new Date(-0.5).getTime()`, "0"},
		{`1 / new Date(-0).getTime()`, "Infinity"},
		{`typeof Date.now()`, `"number"`},
		{`Date.now() === Math.floor(Date.now())`, "true"},
		{`new Date(new Date(5)).getTime()`, "5"},
		{`new Date(new Date(NaN)).getTime()`, "NaN"},
		{`typeof Date()`, `"string"`},
		{`new Date(undefined).getTime()`, "NaN"},
		{`new Date(null).getTime()`, "0"},
		{`new Date(true).getTime()`, "1"},
		{`new Date({valueOf(){ return 7 }}).getTime()`, "7"},
		{`new Date({toString(){ return "1970-01-01T00:00:00.009Z" }, valueOf: undefined}).getTime()`, "9"},
		{`new Date({valueOf(){ return "1970-01-01T00:00:00.009Z" }}).getTime()`, "9"},
		{`new Date(new String("1970-01-01T00:00:00.009Z")).getTime()`, "9"},
		{`new Date(0).valueOf()`, "0"},
		{`new Date(0).toJSON()`, `"1970-01-01T00:00:00.000Z"`},
		{`new Date(NaN).toJSON()`, "null"},
		{`new Date(0) == new Date(0)`, "false"},
		{`new Date(0) <= new Date(0)`, "true"},
		{`new Date(5) < new Date(6)`, "true"},
		{`new Date(5) - 0`, "5"},
		{`Date.prototype.getTime.call({})`, "throws TypeError: Date.prototype.getTime requires that 'this' be a Date object"},
		{`Date.prototype.toISOString.call(0)`, "throws TypeError: Date.prototype.toISOString requires that 'this' be a Date object"},
		{`new Date(0).toISOString.call(undefined)`, "throws TypeError: Date.prototype.toISOString requires that 'this' be a Date object"},
		{`Date.prototype.toJSON.call({toISOString(){return 1}})`, "1"},
		{`Date.prototype.toJSON.call({toISOString(){return 1}, valueOf(){ return NaN }})`, "null"},
		{`Date.prototype.toJSON.call({})`, "throws TypeError: undefined is not a function"},
		{`Date.prototype.toJSON.call(null)`, "throws TypeError: Cannot convert undefined or null to object"},
		{`Date.length`, "7"},
		{`Date.name`, `"Date"`},
		{`Date.prototype.constructor === Date`, "true"},
		{`new Date(0) instanceof Date`, "true"},
		{`Object.prototype.toString.call(new Date(0))`, `"[object Date]"`},
		{`Date.prototype.toJSON.length`, "1"},
		{`Date.now.length`, "0"},
		{`String(new Date(0).getTime)`, `"function getTime() { [native code] }"`},
	})
}

func TestAuditB_SpecURI(t *testing.T) {
	auditBTable(t, [][2]string{
		{`encodeURIComponent("\ud800")`, "throws URIError: URI malformed"},
		{`encodeURIComponent("\udc00")`, "throws URIError: URI malformed"},
		{`encodeURIComponent("a\ud800")`, "throws URIError: URI malformed"},
		{`encodeURIComponent("\ud800a")`, "throws URIError: URI malformed"},
		{`encodeURI("\ud800")`, "throws URIError: URI malformed"},
		{`encodeURIComponent("😀")`, `"%F0%9F%98%80"`},
		{`encodeURIComponent("é")`, `"%C3%A9"`},
		{`encodeURIComponent("€")`, `"%E2%82%AC"`},
		{`encodeURIComponent("\ufffd")`, `"%EF%BF%BD"`},
		{`encodeURIComponent("\u007f")`, `"%7F"`},
		{`encodeURIComponent("\u0000")`, `"%00"`},
		{`encodeURI("http://a/b c?d=é#f")`, `"http://a/b%20c?d=%C3%A9#f"`},
		{`encodeURI("#")`, `"#"`},
		{`encodeURI(";/?:@&=+$,#-_.!~*'()")`, `";/?:@&=+$,#-_.!~*'()"`},
		{`encodeURI("[]{}|\\^\"<>%")`, `"%5B%5D%7B%7D%7C%5C%5E%22%3C%3E%25"`},
		{`encodeURIComponent("#")`, `"%23"`},
		{`encodeURIComponent(";/?:@&=+$,")`, `"%3B%2F%3F%3A%40%26%3D%2B%24%2C"`},
		{`encodeURIComponent("-_.!~*'()")`, `"-_.!~*'()"`},
		{`encodeURIComponent("aZ09")`, `"aZ09"`},
		{`encodeURIComponent()`, `"undefined"`},
		{`encodeURIComponent(null)`, `"null"`},
		{`decodeURIComponent("%E2%82")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%E2%82%AC")`, `"€"`},
		{`decodeURIComponent("%")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%4")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%zz")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%4z")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%C3%A")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%C3A9")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%C3%29")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%FF")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%F8%80%80%80%80")`, "throws URIError: URI malformed"},
		{`decodeURI("%23")`, `"%23"`},
		{`decodeURI("%3B%2F%3F%3A%40%26%3D%2B%24%2C%23")`, `"%3B%2F%3F%3A%40%26%3D%2B%24%2C%23"`},
		{`decodeURI("%3b%2f")`, `"%3b%2f"`},
		{`decodeURI("%41%20")`, `"A "`},
		{`decodeURI("%ED%A0%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%23")`, `"#"`},
		{`decodeURIComponent("%C0%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%C1%BF")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%E0%80%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%ED%A0%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%ED%BF%BF")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%F4%90%80%80")`, "throws URIError: URI malformed"},
		{`decodeURIComponent("%F4%8F%BF%BF") === "\udbff\udfff"`, "true"},
		{`decodeURIComponent("%F0%9F%98%80")`, `"😀"`},
		{`decodeURIComponent("%F0%90%80%80") === "\ud800\udc00"`, "true"},
		{`decodeURIComponent("%41")`, `"A"`},
		{`decodeURIComponent("%41%C3%A9")`, `"Aé"`},
		{`decodeURIComponent("é%41")`, `"éA"`},
		{`decodeURIComponent("\ud800%41").length`, "2"},
		{`decodeURIComponent("%2525")`, `"%25"`},
		{`decodeURIComponent("abc")`, `"abc"`},
		{`decodeURIComponent("")`, `""`},
		{`decodeURIComponent("%C2%80")`, `"` + "\u0080" + `"`},
		{`decodeURIComponent("%EF%BF%BE")`, `"` + "\ufffe" + `"`},
	})
}

// --- E: fail loudly ----------------------------------------------------------------------------

func TestAuditB_FailLoudly(t *testing.T) {
	auditBTable(t, [][2]string{
		{`[..."aba".matchAll(/a/g)].map(m => m.index)`, "[0,2]"},
		{`"\u212b".normalize() === "\u00c5"`, "true"}, // normalize
		{`"a".normalize("nfc")`, "throws RangeError: The normalization form should be one of NFC, NFD, NFKC, NFKD."},
		{`"a".localeCompare("b", "de")`, "-1"}, // locale ignored: CLDR root order (engine/collate.go)
		{`"ä".localeCompare("z", "de")`, "-1"}, // as ICU's root and de collations
		{`"a".toLocaleString()`, `"a"`},        // Object.prototype.toLocaleString -> toString, spec-correct
		{`"a".toLocaleUpperCase()`, `"A"`},
		{`"a".toLocaleLowerCase()`, `"a"`},
		{`String.raw`, "function"},
		{`String.raw({raw: ["a"]})`, `"a"`},
		{`String.fromCodePoint(65)`, `"A"`},
		{`"ba".search(/a/)`, "1"},
		{`"a".isWellFormed()`, "true"},
		{`"a".toWellFormed()`, `"a"`},
		{`"a".trimLeft()`, `"a"`},
		{`"a".anchor("x")`, `"<a name=\"x\">a</a>"`},
		{`new Date(0).getUTCFullYear()`, "1970"}, // the full Date builtin
		{`new Date(0).setTime(5)`, "5"},
		{`Date.UTC(2024, 0, 1)`, "1704067200000"},
		{`Date.parse("2024-01-01")`, "1704067200000"},
		{`typeof Symbol.iterator`, `"symbol"`},
		{`typeof Symbol`, `"function"`},
		{`typeof Intl.Segmenter`, `"function"`},
		{`typeof Intl.NumberFormat`, `"function"`},
		{`typeof Intl.Collator`, `"undefined"`},
		{`BigInt(1)`, "1n"},
		{`1n + 1n`, "2n"},
		{`1n * 2n`, "2n"},
		{`-1n`, "-1n"},
		{`1n + 1`, "throws TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{`JSON.rawJSON`, "undefined"},
		{`JSON.isRawJSON`, "undefined"},
		{`structuredClone({a: [1]})`, `{"a":[1]}`},
		{`new TextDecoder().decode(new TextEncoder().encode("é"))`, `"é"`},
		{`atob("YQ==")`, `"a"`}, // forgiving-base64 (builtin_uri.go)
		{`btoa("a")`, `"YQ=="`},
		{`String.prototype.toString.call({})`, "throws TypeError: String.prototype.toString requires that 'this' be a String"},
		{`String.prototype.valueOf.call(1)`, "throws TypeError: String.prototype.valueOf requires that 'this' be a String"},
	})
}

// --- A: aliasing of string storage ------------------------------------------------------------

func TestAuditB_NoAliasingOfPublishedStrings(t *testing.T) {
	units := []uint16{' ', 'A', 'b', 0xE9, 'C', 0xDF, 0x3A3, ' ', 0xD83D, 0xDE00, ' '}
	orig := append([]uint16(nil), units...)
	s := FromUTF16(units)
	snapshot := func() []uint16 { return append([]uint16(nil), s.UTF16()...) }
	require.Equal(t, orig, snapshot())

	r := NewRealm()
	lower, _ := stringToLower(r, s)
	upper, _ := stringToUpper(r, s)
	trimmed := trimString(s, true, true)
	sub := s.Substring(1, 6)
	sv := StringValue(s)
	for _, call := range []func() (Value, error){
		func() (Value, error) {
			return auditBMethod(t, r, sv, "replaceAll", StringValue(FromGoString("b")), StringValue(FromGoString("ZZ")))
		},
		func() (Value, error) { return auditBMethod(t, r, sv, "split", StringValue(FromGoString(" "))) },
		func() (Value, error) {
			return auditBMethod(t, r, sv, "padStart", IntValue(20), StringValue(FromGoString("é")))
		},
		func() (Value, error) { return auditBMethod(t, r, sv, "padEnd", IntValue(20), sv) },
		func() (Value, error) { return auditBMethod(t, r, sv, "repeat", IntValue(3)) },
		func() (Value, error) { return auditBMethod(t, r, sv, "concat", sv, sv) },
		func() (Value, error) { return auditBMethod(t, r, sv, "trimEnd") },
		func() (Value, error) { return auditBMethod(t, r, ObjectValue(r.JSON), "stringify", sv) },
		func() (Value, error) {
			fn, _ := r.Global.GetProp(r, r.KeyFromGoString("encodeURIComponent"))
			return r.Call(fn, Undefined(), []Value{StringValue(s.Substring(0, 8))})
		},
	} {
		_, err := call()
		require.NoError(t, err)
		assert.Equal(t, orig, snapshot(), "source string mutated by a derived method")
	}
	concat, err := r.Concat(s, s)
	require.NoError(t, err)
	_ = concat.At(3) // flatten
	assert.Equal(t, orig, snapshot())

	lowerU := append([]uint16(nil), lower.UTF16()...)
	upperU := append([]uint16(nil), upper.UTF16()...)
	_, _ = stringToUpper(r, lower)
	_, _ = stringToLower(r, upper)
	_ = trimString(trimmed, true, true)
	_ = sub.Substring(0, 2)
	assert.Equal(t, lowerU, lower.UTF16())
	assert.Equal(t, upperU, upper.UTF16())
	assert.Equal(t, orig[1:6], sub.UTF16())
	assert.Equal(t, orig[1:10], trimmed.UTF16())

	// Substring shares the parent's storage (documented zero-copy); the parent
	// is not disturbed by operations on the child.
	child := s.Substring(2, 5)
	_, _ = stringToUpper(r, child)
	childConcat, err := r.Concat(child, FromGoString("é"))
	require.NoError(t, err)
	_ = childConcat.GoString()
	assert.Equal(t, orig, snapshot())

	// StringBuilder: String() hands out sb.u; further writes must not touch
	// the published result (UTF-16, spilled ASCII and inline ASCII paths).
	var sb StringBuilder
	sb.WriteString(FromUTF16([]uint16{0xE9, 'a', 'b'}))
	first := sb.String()
	firstU := append([]uint16(nil), first.UTF16()...)
	sb.WriteUnit('Z')
	sb.WriteString(FromGoString("more"))
	second := sb.String()
	assert.Equal(t, firstU, first.UTF16())
	assert.Equal(t, "éab", first.GoString())
	assert.Equal(t, "Zmore", second.GoString())

	var sb2 StringBuilder
	sb2.WriteGoString(auditBAlpha(65))
	a := sb2.String()
	aStr := a.GoString()
	sb2.WriteGoString("tail")
	b := sb2.String()
	assert.Equal(t, aStr, a.GoString())
	assert.Equal(t, "tail", b.GoString())

	var sb3 StringBuilder
	sb3.WriteGoString("short")
	c := sb3.String()
	sb3.WriteGoString("XXXXX")
	d := sb3.String()
	assert.Equal(t, "short", c.GoString())
	assert.Equal(t, "XXXXX", d.GoString())

	// A builder upgraded mid-way (ASCII then UTF-16) after handing out a
	// spilled ASCII result.
	var sb4 StringBuilder
	sb4.WriteGoString(auditBAlpha(70))
	e := sb4.String()
	eStr := e.GoString()
	sb4.WriteGoString("x")
	sb4.WriteUnit(0xE9)
	f := sb4.String()
	assert.Equal(t, eStr, e.GoString())
	assert.Equal(t, "xé", f.GoString())

	// Case conversion: UTF-16 input whose result is ASCII (FromUTF16 copies),
	// and the co-allocated ASCII newASCIIBuf paths.
	k := FromUTF16([]uint16{0x212A, 'B'})
	kl, _ := stringToLower(r, k)
	assert.Equal(t, "kb", kl.GoString())
	assert.Equal(t, []uint16{0x212A, 'B'}, k.UTF16())
	asc := FromGoString("Hello World")
	up, _ := stringToUpper(r, asc)
	lo, _ := stringToLower(r, asc)
	assert.Equal(t, "Hello World", asc.GoString())
	assert.Equal(t, "HELLO WORLD", up.GoString())
	assert.Equal(t, "hello world", lo.GoString())
	_, _ = stringToLower(r, up)
	_, _ = stringToUpper(r, lo)
	assert.Equal(t, "HELLO WORLD", up.GoString())
	assert.Equal(t, "hello world", lo.GoString())
	// ASCII Substring shares the Go string (immutable): fine by construction.
	assert.Equal(t, "ello", asc.Substring(1, 5).GoString())
}

func auditBAlpha(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}

// --- F: export parity ----------------------------------------------------------------------------

func TestAuditB_GoStringExport(t *testing.T) {
	assert.Equal(t, "\uFFFD", FromUTF16([]uint16{0xD800}).GoString())
	assert.Equal(t, "\uFFFDa", FromUTF16([]uint16{0xDC00, 'a'}).GoString())
	assert.Equal(t, "\U0001F600", FromUTF16([]uint16{0xD83D, 0xDE00}).GoString())
	assert.Len(t, FromUTF16([]uint16{0xD83D, 0xDE00}).GoString(), 4)
	// Left-deep (s += piece), right-deep (s = piece + s) and mixed ropes of
	// 1e5 pieces: flatten is iterative and must not overflow the Go stack.
	for _, body := range []string{
		`let s = ""; for (let i = 0; i < 100000; i++) s += "ab"; return s;`,
		`let s = ""; for (let i = 0; i < 100000; i++) s = "ab" + s; return s;`,
		`let s = ""; for (let i = 0; i < 100000; i++) s += "é"; return s;`,
		`let s = ""; for (let i = 0; i < 100000; i++) s = (i % 2 ? "é" : "a") + s + "b"; return s;`,
	} {
		_, v, err := auditBRun(t, body)
		require.NoError(t, err)
		require.True(t, v.IsString())
		g := v.AsString().GoString()
		assert.NotEmpty(t, g)
		if v.AsString().IsASCII() {
			assert.Len(t, g, 200000)
		}
	}
}

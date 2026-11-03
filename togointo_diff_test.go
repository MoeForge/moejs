package moejs_test

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/require"
)

// --- ToGoInto differential ----------------------------------------------------
// JavaScript values x Go targets, in a shared and a mutable runtime,
// ToGoInto against json.Unmarshal of json.Marshal of ToGo's result: errors
// and targets alike (-0 told from 0), and the target as it was when ToGo or
// json.Marshal fails.

type tgMerge struct {
	A struct{ I, J int }
}

var tgExprs = []string{
	`2**53`, `2**63-1024`, `-(2**63)-2048`, `1e-7`, `-1e-7`, `0.000001`, `1e20`, `123456789012345680000`, `5e-324`, `-1.5`,
	`[-0]`, `[1, -0, 0.5]`, `{F64: -0, Any: -0, F32: -0}`, `{I64: 2**63}`, `{I64: -(2**63)}`, `{U64: 2**63}`, `{U64: 2**64}`, `{U: 2**64-2048}`,
	`{N: 2**70}`, `{N: 1e21}`, `{N: 1e-7}`, `{N: 5}`, `{F32: 16777217}`, `{F32: 2**60+2**36}`, `{F32: 2**60+3*2**36}`, `{F32: 3.4028235677973366e38}`, `{F32: 3.4028235677973362e38}`, `{F32: 1e-46}`,
	`{a: NaN}`, `[Infinity]`, `{Known: 1, unknown: -Infinity}`, `{Known: 1, unknown: [1, {x: NaN}]}`,
	`{a: undefined, i: undefined}`, `[undefined, null]`, `{P: undefined, M: {k: undefined}, S: [undefined]}`, `{I: undefined, Any: undefined, Str: undefined}`,
	`{A: {I: 1}, a: {J: 2}}`, `{a: {J: 2}, A: {I: 1}}`, `{a: 1, A: 2}`, `Object.fromEntries([["\ud800", 1], ["\udfff", 2]])`, `Object.fromEntries([["\ud800", {I: 1}], ["\udfff", {J: 2}]])`,
	`new Uint8Array([104, 105])`, `{Bs: new Uint8Array([104, 105])}`, `new ArrayBuffer(2)`, `1n`, `{i: 1n}`, `Symbol("x")`, `{a: Symbol("x")}`, `() => 1`, `{Known: 1, f: function () {}}`,
	`new Map([["a", 1]])`, `new Set([1])`, `{d: new Date(0)}`, `new Number(1)`, `new String("s")`, `{a: new Boolean(false)}`, `Object(1n)`,
	`new Error("x")`, `{e: new Error("x")}`, `(function () { return arguments })(1, 2)`, `/re/g`, `Promise.resolve(1)`,
	`new (class { constructor() { this.i = 1; this.Known = 2 } })()`, `new (class { get x() { return 1 } constructor() { this.i = 2 } })()`,
	`{i: 2, toJSON: 5}`, `{i: 3, toJSON() { return {i: 4} }}`, `Object.setPrototypeOf([1, 2], null)`, `(() => { class A extends Array {}; return A.from([1, 2]) })()`,
	`Object.setPrototypeOf({i: 5}, {j: 6})`, `Object.assign(Object.create({inherited: 1}), {i: 7})`,
	`(() => { const o = {i: 1}; Object.defineProperty(o, "g", {get() { throw new Error("getter") }, enumerable: true}); return o })()`,
	`(() => { const o = {i: 1}; Object.defineProperty(o, "g", {get() { throw new Error("getter") }, enumerable: false}); return o })()`,
	`(() => { const o = {}; o.self = [o]; return o })()`, `(() => { const s = {i: 1}; return {a: s, b: s, c: [s, s]} })()`,
	`(() => HOST)()`, `({k: 1, h: HOST})`, `[HOST, HOST]`, `(() => { const h = HOST; h.messages; return h })()`, `(() => { const h = HOST; h.n = 3; return h })()`,
	`(() => { const h = HOST; h.messages[0].role = "sys"; return h })()`, `(() => { const h = HOST; h.extra = -0; return h })()`, `(() => { const h = HOST; delete h.nilslice; return h })()`,
	`(() => { const h = HOST; return [h.nilslice, h.nilstrs, h.nilmaps, h.nilmap, h.f32, h.neg0, h.multi, h.u8, h.empty] })()`,
	`(() => { const h = HOST; return {Any: h.messages, I: h.n, F64: h.neg0, F32: h.f32, Str: h.model} })()`,
	`(() => BAD)()`, `({Known: 1, unknown: BAD})`, `(() => { const b = BAD; b.n = 1; return b })()`, `(() => { const b = BAD; return [b.f32n] })()`,
	`(() => ODD)()`, `({Known: 1, unknown: ODD})`, `(() => { const o = ODD; o.n = 1; return o })()`,
}

func TestToGoIntoDifferential(t *testing.T) {
	exprs := append(append([]string(nil), umExprs...), tgExprs...)
	var sb strings.Builder
	sb.WriteString("export function v(i, HOST, BAD, ODD) { return [\n")
	for _, e := range exprs {
		sb.WriteString("() => (" + e + "),\n")
	}
	sb.WriteString("][i](); }\n")
	mod, err := moejs.Compile("tg.js", sb.String())
	require.NoError(t, err)
	targets := append(umTargets(), []struct {
		name string
		mk   func() (a, b any)
	}{
		{"tgMerge", func() (any, any) { var a, b tgMerge; return &a, &b }},
		{"slice-float64", func() (any, any) { var a, b []float64; return &a, &b }},
		{"slice-float32", func() (any, any) { var a, b []float32; return &a, &b }},
		{"map-string-float64", func() (any, any) { var a, b map[string]float64; return &a, &b }},
		{"map-string-number", func() (any, any) { var a, b map[string]json.Number; return &a, &b }},
		{"map-string-tgMerge", func() (any, any) { var a, b map[string]tgMerge; return &a, &b }},
	}...)
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
		require.NoError(t, rt.Load(mod))
		hook := mustHook(t, mod, "v")
		from := func(g any) moejs.Value {
			v, err := rt.FromGo(g)
			require.NoError(t, err)
			return v
		}
		args := func(i int) []moejs.Value {
			return []moejs.Value{
				umNum(t, rt, i),
				from(map[string]any{
					"model": "m", "n": 2, "messages": []any{map[string]any{"role": "user", "content": "hi", "x": 1.5}},
					"tags": []string{"a", "b"}, "hdr": map[string]string{"k": "v"}, "big": int64(1<<53 + 1), "u8": uint8(3),
					"f32": float32(0.1), "neg0": math.Copysign(0, -1), "nilslice": []any(nil), "nilstrs": []string(nil),
					"nilmaps": []map[string]any(nil), "nilmap": map[string]any(nil), "empty": []any{}, "multi": map[string][]string{"a": nil, "b": {"x"}},
					"raw": json.RawMessage(` {"z": [1, 2.50, 1E2, -0, "<\u2028>\ud800"], "a": null, "é": {"k": "v"}} `), "rawlist": json.RawMessage(`[1, {"x": "é"}, []]`), "rawprim": json.RawMessage(` "s" `), "rawnull": json.RawMessage(nil),
				}),
				from(map[string]any{"n": 1, "nan": math.NaN(), "f32n": []any{float32(math.Inf(1))}}),
				from(map[string]any{"num": json.Number("1.50"), "bad": "a\xffb", "k\xff": 1}),
			}
		}
		mismatches := 0
		for i, e := range exprs {
			for _, tg := range targets {
				va, err := rt.Call(hook, args(i)...)
				require.NoError(t, err, e)
				vb, err := rt.Call(hook, args(i)...)
				require.NoError(t, err, e)
				ta, tb := tg.mk()
				errA := rt.ToGoInto(va, ta)
				stage := ""
				g, errB := rt.ToGo(vb)
				var data []byte
				if errB != nil {
					stage = "ToGo"
				} else if data, errB = json.Marshal(g); errB != nil {
					stage = "Marshal"
				} else {
					errB = json.Unmarshal(data, tb)
				}
				okErr := umErr(errA) == umErr(errB)
				okVal := tgDump(ta) == tgDump(tb)
				fresh, _ := tg.mk()
				okKept := stage == "" || tgDump(ta) == tgDump(fresh)
				if !okErr || !okVal || !okKept {
					mismatches++
					t.Errorf("mutable=%v value %s into %s:\n  ToGoInto:  err=%v val=%s\n  roundtrip: err=%v (%s) val=%s text=%s", mutable, e, tg.name, errA, tgDump(ta), errB, stage, tgDump(tb), data)
				}
			}
		}
		t.Logf("mutable=%v: %d exprs x %d targets, %d mismatches", mutable, len(exprs), len(targets), mismatches)
	}
}

// tgDump writes v down to its last pointer, -0 apart from 0, interfaces
// with their dynamic types, maps in key order.
func tgDump(v any) string {
	var sb strings.Builder
	tgDumpValue(&sb, reflect.ValueOf(v))
	return sb.String()
}

func tgDumpValue(sb *strings.Builder, rv reflect.Value) {
	if !rv.IsValid() {
		sb.WriteString("invalid")
		return
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			sb.WriteString("nil")
			return
		}
		sb.WriteString("&")
		tgDumpValue(sb, rv.Elem())
	case reflect.Interface:
		if rv.IsNil() {
			sb.WriteString("nil-iface")
			return
		}
		sb.WriteString(rv.Elem().Type().String() + "(")
		tgDumpValue(sb, rv.Elem())
		sb.WriteString(")")
	case reflect.Map:
		if rv.IsNil() {
			sb.WriteString("nil-map")
			return
		}
		keys := rv.MapKeys()
		ks := make([]string, len(keys))
		byKey := map[string]reflect.Value{}
		for i, k := range keys {
			var kb strings.Builder
			tgDumpValue(&kb, k)
			ks[i] = kb.String()
			byKey[ks[i]] = rv.MapIndex(k)
		}
		sort.Strings(ks)
		sb.WriteString("{")
		for _, k := range ks {
			sb.WriteString(k + ":")
			tgDumpValue(sb, byKey[k])
			sb.WriteString(",")
		}
		sb.WriteString("}")
	case reflect.Slice:
		if rv.IsNil() {
			sb.WriteString("nil-slice")
			return
		}
		fallthrough
	case reflect.Array:
		sb.WriteString("[")
		for i := range rv.Len() {
			tgDumpValue(sb, rv.Index(i))
			sb.WriteString(",")
		}
		sb.WriteString("]")
	case reflect.Struct:
		sb.WriteString(rv.Type().String() + "{")
		for i := range rv.NumField() {
			sb.WriteString(rv.Type().Field(i).Name + ":")
			tgDumpValue(sb, rv.Field(i))
			sb.WriteString(",")
		}
		sb.WriteString("}")
	case reflect.Bool:
		sb.WriteString(strconv.FormatBool(rv.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		sb.WriteString(strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		sb.WriteString(strconv.FormatUint(rv.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		sb.WriteString(strconv.FormatFloat(rv.Float(), 'g', -1, 64))
	case reflect.String:
		sb.WriteString(strconv.Quote(rv.String()))
	default:
		sb.WriteString(rv.Kind().String())
	}
}

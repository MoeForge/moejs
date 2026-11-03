package moejs_test

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/require"
)

// --- differential ----------------------------------------------------------------
// From the audit of the first version (which returned nil where the round
// trip failed inside a discarded value): JavaScript values x Go targets, in a
// shared and a mutable runtime, Unmarshal against json.Unmarshal of
// AppendJSON's text, errors and targets alike.

type umInner struct {
	I int `json:"i"`
}

type umEmbedded struct {
	umInner
	B int
}

type umTextU struct{ S string }

func (x *umTextU) UnmarshalText(b []byte) error { x.S = "text:" + string(b); return nil }

type umJSONU struct{ S string }

func (x *umJSONU) UnmarshalJSON(b []byte) error { x.S = "json:" + string(b); return nil }

type umWithString struct {
	A int `json:"a,string"`
}

type umTagged struct {
	A int `json:"x"`
	B int `json:"-"`
	C int `json:"-,"`
	D int `json:",omitempty"`
	E int `json:"e,omitempty"`
	f int
	G *int
	H **int
	K int `json:"k"`
	S int `json:"s"`
	Ä int `json:"ä"`
}

type umDominance struct {
	A int
	B int `json:"a"`
}

type umDominance2 struct {
	Name string
	NAME string
}

type umDominance3 struct {
	Name string `json:"name"`
	NAME string
}

type umPointers struct {
	P  *int
	PP **umInner
	M  map[string]*umInner
	S  []*umInner
	A  [2]*int
}

type umArrays struct {
	A2 [2]int
	A4 [4]int
	A0 [0]int
	AA [3]any
}

type umNumbers struct {
	I8  int8
	I   int
	I64 int64
	U   uint
	U8  uint8
	U64 uint64
	F32 float32
	F64 float64
	N   json.Number
	B   bool
	Str string
	Raw json.RawMessage
	Bs  []byte
	Any any
}

// umRaw holds json.RawMessage in every position the walk writes one.
type umRaw struct {
	Raw json.RawMessage
	P   *json.RawMessage
	M   map[string]json.RawMessage
	L   []json.RawMessage
	In  struct{ R json.RawMessage }
	I   int `json:"i"`
}

type umStringer interface{ String() string }

type umIfaces struct {
	E any
	S umStringer
	T encoding.TextUnmarshaler
}

func umTargets() []struct {
	name string
	mk   func() (a, b any)
} {
	five := 5
	pint := func() *int { x := 5; return &x }
	return []struct {
		name string
		mk   func() (a, b any)
	}{
		{"any", func() (any, any) { var a, b any; return &a, &b }},
		{"any-holding-int", func() (any, any) { var a, b any = 5, 5; return &a, &b }},
		{"any-holding-map", func() (any, any) {
			var a, b any = map[string]any{"keep": 1}, map[string]any{"keep": 1}
			return &a, &b
		}},
		{"any-holding-ptrmap", func() (any, any) {
			ma, mb := map[string]any{"keep": 1}, map[string]any{"keep": 1}
			var a, b any = &ma, &mb
			return &a, &b
		}},
		{"any-holding-ptrint", func() (any, any) { var a, b any = pint(), pint(); return &a, &b }},
		{"any-holding-ptrstruct", func() (any, any) { var a, b any = &umInner{I: 9}, &umInner{I: 9}; return &a, &b }},
		{"any-holding-slice", func() (any, any) { var a, b any = []any{1, 2, 3}, []any{1, 2, 3}; return &a, &b }},
		{"int", func() (any, any) { a, b := 7, 7; return &a, &b }},
		{"int8", func() (any, any) { var a, b int8; return &a, &b }},
		{"uint", func() (any, any) { var a, b uint; return &a, &b }},
		{"float32", func() (any, any) { var a, b float32; return &a, &b }},
		{"float64", func() (any, any) { var a, b float64; return &a, &b }},
		{"string", func() (any, any) { a, b := "keep", "keep"; return &a, &b }},
		{"bool", func() (any, any) { a, b := true, true; return &a, &b }},
		{"bytes", func() (any, any) { var a, b []byte; return &a, &b }},
		{"jsonnumber", func() (any, any) { var a, b json.Number; return &a, &b }},
		{"raw", func() (any, any) { var a, b json.RawMessage; return &a, &b }},
		{"raw-prefilled", func() (any, any) {
			a, b := append(make(json.RawMessage, 0, 256), "old"...), append(make(json.RawMessage, 0, 256), "old"...)
			return &a, &b
		}},
		{"ptr-raw", func() (any, any) { var a, b *json.RawMessage; return &a, &b }},
		{"map-string-raw", func() (any, any) { var a, b map[string]json.RawMessage; return &a, &b }},
		{"slice-raw", func() (any, any) { var a, b []json.RawMessage; return &a, &b }},
		{"slice-raw-prefilled", func() (any, any) {
			a, b := []json.RawMessage{json.RawMessage("x"), nil, json.RawMessage("y")}, []json.RawMessage{json.RawMessage("x"), nil, json.RawMessage("y")}
			return &a, &b
		}},
		{"umRaw", func() (any, any) { var a, b umRaw; return &a, &b }},
		{"umTextU", func() (any, any) { var a, b umTextU; return &a, &b }},
		{"umJSONU", func() (any, any) { var a, b umJSONU; return &a, &b }},
		{"ptr-int-nil", func() (any, any) { var a, b *int; return &a, &b }},
		{"ptr-int-set", func() (any, any) { a, b := pint(), pint(); return &a, &b }},
		{"ptrptr-int", func() (any, any) { var a, b **int; return &a, &b }},
		{"map-string-any", func() (any, any) { var a, b map[string]any; return &a, &b }},
		{"map-string-any-prefilled", func() (any, any) {
			a, b := map[string]any{"keep": 1, "a": "old"}, map[string]any{"keep": 1, "a": "old"}
			return &a, &b
		}},
		{"map-string-int", func() (any, any) { var a, b map[string]int; return &a, &b }},
		{"map-int-string", func() (any, any) { var a, b map[int]string; return &a, &b }},
		{"map-string-ptrint", func() (any, any) { var a, b map[string]*int; return &a, &b }},
		{"map-string-umInner", func() (any, any) { var a, b map[string]umInner; return &a, &b }},
		{"map-string-umTextU", func() (any, any) { var a, b map[string]umTextU; return &a, &b }},
		{"map-umTextU-int", func() (any, any) { var a, b map[umTextU]int; return &a, &b }},
		{"slice-int", func() (any, any) { var a, b []int; return &a, &b }},
		{"slice-int-prefilled", func() (any, any) { a, b := []int{9, 9, 9, 9}, []int{9, 9, 9, 9}; return &a, &b }},
		{"slice-umInner-prefilled", func() (any, any) {
			a, b := []umInner{{I: 9}, {I: 8}}, []umInner{{I: 9}, {I: 8}}
			return &a, &b
		}},
		{"slice-any", func() (any, any) { var a, b []any; return &a, &b }},
		{"slice-string", func() (any, any) { var a, b []string; return &a, &b }},
		{"slice-ptrint", func() (any, any) { var a, b []*int; return &a, &b }},
		{"array-2", func() (any, any) { var a, b [2]int; return &a, &b }},
		{"array-4-prefilled", func() (any, any) { a, b := [4]int{9, 9, 9, 9}, [4]int{9, 9, 9, 9}; return &a, &b }},
		{"array-1", func() (any, any) { var a, b [1]int; return &a, &b }},
		{"array-0", func() (any, any) { var a, b [0]int; return &a, &b }},
		{"array-3-any", func() (any, any) { var a, b [3]any; return &a, &b }},
		{"umInner", func() (any, any) { var a, b umInner; return &a, &b }},
		{"umInner-prefilled", func() (any, any) { a, b := umInner{I: 9}, umInner{I: 9}; return &a, &b }},
		{"umEmbedded", func() (any, any) { var a, b umEmbedded; return &a, &b }},
		{"umWithString", func() (any, any) { var a, b umWithString; return &a, &b }},
		{"umTagged", func() (any, any) { a, b := umTagged{f: 1, G: &five}, umTagged{f: 1, G: &five}; return &a, &b }},
		{"umDominance", func() (any, any) { var a, b umDominance; return &a, &b }},
		{"umDominance2", func() (any, any) { var a, b umDominance2; return &a, &b }},
		{"umDominance3", func() (any, any) { var a, b umDominance3; return &a, &b }},
		{"umPointers", func() (any, any) { var a, b umPointers; return &a, &b }},
		{"umArrays", func() (any, any) { var a, b umArrays; return &a, &b }},
		{"umNumbers", func() (any, any) { var a, b umNumbers; return &a, &b }},
		{"umIfaces", func() (any, any) { var a, b umIfaces; return &a, &b }},
		{"struct-known-only", func() (any, any) { var a, b struct{ Known int }; return &a, &b }},
		{"struct-name", func() (any, any) { var a, b struct{ Name string }; return &a, &b }},
		{"struct-K-S", func() (any, any) { var a, b struct{ K, S int }; return &a, &b }},
		{"not-pointer", func() (any, any) { var a, b int; return a, b }},
		{"nil-pointer", func() (any, any) { var a, b *int; return a, b }},
		{"nil-any", func() (any, any) { return nil, nil }},
	}
}

var umExprs = []string{
	`1`, `1.5`, `-0`, `1e21`, `2**53+2`, `2**63`, `-(2**63)`, `2**64`, `1e400`, `NaN`, `300`, `-1`, `1e39`, `0.1`, `255`, `256`, `3.4028235e38`,
	`"s"`, `"\ud800"`, `"\udc00\ud800"`, `"a\u2028b"`, `"aGVsbG8="`, `"not base64!"`, `"12"`, `"1.5"`, `"abc"`, `""`, `"😀"`,
	`true`, `null`, `undefined`,
	`[]`, `[1,"a",null,undefined,()=>1, Symbol()]`, `[1,2,3]`, `[1,2]`, `[1]`, `[1,2,3,4,5]`, `["1","2"]`, `[[1],[2,3]]`, `[{i:1},{i:2},{i:3}]`, `[null, 1]`, `[1, {toJSON(){ throw new Error("boom") }}]`, `[1, 2, {toJSON(){ throw new Error("boom") }}]`, `[1, 1n]`, `[1, [2, {a: 1n}]]`,
	`[1, (()=>{const o={a:1}; o.self=o; return o})()]`, `[1, 2, (()=>{const o={a:1}; o.self=o; return o})()]`, `[1, {d: new Date(0)}]`, `[1, 2, {get g() { globalThis.sideEffect = (globalThis.sideEffect||0)+1; return 1 }}]`,
	`{}`, `{a:1,A:2}`, `{name:"x",Name:"y"}`, `{"NAME":"z"}`, `{a:undefined}`, `{a:()=>1}`, `{a: {b: [1,2,{c:"d"}]}}`, `{"0":1, b:2}`, `{i: 1}`, `{i: "1"}`, `{i: 1.5}`, `{i: 1e100}`,
	`Object.assign(Object.create(null), {i: 1})`, `Object.defineProperty({}, 'i', {get(){return 1}, enumerable:true})`, `Object.defineProperty({i: 2}, 'hidden', {value: 1, enumerable: false})`,
	`{toJSON(){return {i:1}}}`, `{a: {toJSON(){ throw new Error("boom") }}}`, `{a: 1n}`, `(()=>{const o={i:1}; o.self=o; return o})()`, `new Date(0)`, `{d: new Date(0)}`, `new Map()`, `new Proxy({i:1},{})`,
	`{unknown: {toJSON(){throw new Error("boom")}}, known: 1, Known: 2}`, `{unknown: (()=>{const o={a:1}; o.self=o; return o})(), known: 1}`, `{unknown: 1n, known: 1}`, `{Known: 1, unknown: {deep: {deeper: 1n}}}`, `{unknown: new Proxy({}, {ownKeys() { throw new Error("trap") }}), Known: 3}`,
	`{Known: 1, unknown: {get g() { globalThis.sideEffect2 = (globalThis.sideEffect2||0)+1; return 1 }}}`,
	`{x: 1, B: 2, "-": 3, D: 4, e: 5, f: 6, G: 7, H: 8, "\u212a": 9, "ſ": 10, "Ä": 11}`, `{A: 1, a: 2}`, `{a: 1}`, `{name: "n", NAME: "N"}`, `{Name: "n"}`,
	`{P: 1, PP: {i: 2}, M: {k: {i: 3}, n: null}, S: [{i: 4}, null], A: [1, null]}`, `{P: null, PP: null, M: null, S: null, A: null}`,
	`{A2: [1,2,3], A4: [1,2], A0: [1], AA: [1,"a",{x:1},4]}`, `{A2: [1, {toJSON(){throw new Error("boom")}}, 3]}`, `{A0: [{toJSON(){throw new Error("boom")}}]}`,
	`{I8: 1, I: 2, I64: 2**53+2, U: 3, U8: 4, U64: 2**64, F32: 0.1, F64: 1e300, N: 1.5, B: true, Str: "s", Raw: {z: [1]}, Bs: "aGk=", Any: [1, {a: "b"}]}`,
	`{I8: 300}`, `{I8: "1"}`, `{U: -1}`, `{F32: 1e39}`, `{N: "1.5"}`, `{N: "abc"}`, `{Str: 1}`, `{B: 1}`, `{Any: 1}`, `{Any: 2**53+2}`, `{Any: {n: -0}}`, `{Bs: "***"}`, `{Bs: [1,2]}`, `{Raw: null}`, `{I64: -0}`, `{U64: -0}`, `{F32: -0}`, `{N: -0}`,
	`{E: {a: 1}, S: "x", T: "y"}`, `{E: null, S: null, T: null}`,
	`(() => { const h = HOST; return h })()`, `(() => ({body: HOST, n: 1}))()`, `(() => { const h = HOST; h.messages; return h })()`, `(() => { const h = HOST; h.messages[0].role = "sys"; return h })()`, `(() => [HOST, HOST])()`, `(() => { const h = HOST; delete h.n; return h })()`,
	`(() => ({i: 1, j: HOST}))()`, `(() => { const o = {a: 1, b: 2, c: 3}; delete o.b; return o })()`, `(() => { const o = {i: 1, b: 2, c: 3}; delete o.b; o.i = 4; return o })()`,
	`(() => { const a = [1,2,3]; a.length = 5; return a })()`, `(() => { const a = [1,,3]; return a })()`, `(() => { const a = [1,2]; a.x = 1; return a })()`, `(() => { const a = []; a[2] = 1; return a })()`,
	`{i: 1, "": 2}`, `{"": 1}`, `{"i\u0000": 1}`, `Object.fromEntries([["ab", 1], ["\ud800", 2]])`,
	`(() => { const o = {}; for (let k = 0; k < 40; k++) o["k" + k] = k; o.i = 7; return o })()`,
	`(() => { const o = {i: 1}; Object.defineProperty(o, Symbol.iterator, {value: 1, enumerable: true}); return o })()`,
	`(() => { const o = {known: 1}; o.toJSON = undefined; return o })()`, `(() => { const o = {known: 1}; o.toJSON = 5; return o })()`,
	`{Known: 1, i: Object.assign(() => 1, {toJSON() { return 5 }})}`,
	`{Raw: {s: "<>&\u2028\u2029", l: "\ud800x\udc00", u: "é😀\u0000\u001f\"\\/"}, P: [-0, 1e21, 5e-324, 1e-7, 2**53+1, 0.1], M: {a: {b: [1, {c: undefined}]}, n: null, u: undefined}, L: [1, "s", null, undefined, {x: [true]}], In: {R: {z: 1, a: 2}}, i: 3}`,
	`{Raw: HOST.raw, P: HOST.rawlist, M: {a: HOST.raw, p: HOST.rawprim}, L: [HOST.rawlist, HOST.raw], In: {R: HOST}}`,
	`(() => { const r = HOST.raw; r.z; return {Raw: r, P: r.z, L: [r.a]} })()`,
	`(() => { const r = HOST.raw; r.extra = 1; return {Raw: r, M: {r}} })()`,
	`(() => HOST.raw)()`, `(() => HOST.rawlist)()`, `(() => [HOST.rawprim, HOST.rawnull])()`, `{M: HOST.hdr, L: HOST.tags, Raw: HOST.messages}`,
	`{Raw: {toJSON() { return {t: 1} }}, i: 1}`, `{Raw: new Date(0)}`, `{P: 1n}`, `[Object.assign(() => 1, {toJSON() { return 6 }}), 2]`, `{unknown: [1, {a: Object.assign(() => 1, {toJSON() { throw new Error("fn") }})}], Known: 1}`,
}

func TestUnmarshalDifferential(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("export function v(i, HOST) { return [\n")
	for _, e := range umExprs {
		sb.WriteString("() => (" + e + "),\n")
	}
	sb.WriteString("][i](); }\n")
	mod, err := moejs.Compile("u.js", sb.String())
	require.NoError(t, err)
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
		require.NoError(t, rt.Load(mod))
		hook := mustHook(t, mod, "v")
		mkHost := func() moejs.Value {
			h, err := rt.FromGo(map[string]any{"model": "m", "n": 2, "messages": []any{map[string]any{"role": "user", "content": "hi", "x": 1.5}}, "tags": []string{"a", "b"}, "hdr": map[string]string{"k": "v"}, "big": int64(1<<53 + 1), "nan": 0.0, "f32": []any{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))},
				"raw": json.RawMessage(` {"z": [1, 2.50, 1E2, -0, "<\u2028>\ud800"], "a": null, "é": {"k": "v"}} `), "rawlist": json.RawMessage(`[1, {"x": "é"}, []]`), "rawprim": json.RawMessage(` "s" `), "rawnull": json.RawMessage(nil)})
			require.NoError(t, err)
			return h
		}
		mismatches := 0
		for i, e := range umExprs {
			for _, tg := range umTargets() {
				va, err := rt.Call(hook, umNum(t, rt, i), mkHost())
				require.NoError(t, err, e)
				vb, err := rt.Call(hook, umNum(t, rt, i), mkHost())
				require.NoError(t, err, e)
				ta, tb := tg.mk()
				errA := rt.Unmarshal(va, ta)
				data, errB := rt.AppendJSON(nil, vb)
				if errB == nil {
					errB = json.Unmarshal(data, tb)
				}
				okErr := umErr(errA) == umErr(errB)
				okVal := reflect.DeepEqual(ta, tb)
				if !okErr || !okVal {
					mismatches++
					t.Errorf("mutable=%v value %s into %s:\n  Unmarshal: err=%v val=%s\n  roundtrip: err=%v val=%s text=%s", mutable, e, tg.name, errA, umFmt(ta), errB, umFmt(tb), data)
				}
			}
		}
		t.Logf("mutable=%v: %d exprs x %d targets, %d mismatches", mutable, len(umExprs), len(umTargets()), mismatches)
	}
}

func umFmt(v any) string {
	if v == nil {
		return "nil"
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "nil-ptr"
		}
		v = rv.Elem().Interface()
	}
	return fmt.Sprintf("%#v", v)
}

func umErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestAuditRace: runtimes in parallel sharing compiled patterns and modules.
func umNum(t *testing.T, rt *moejs.Runtime, i int) moejs.Value {
	t.Helper()
	v, err := rt.FromGo(i)
	require.NoError(t, err)
	return v
}

// TestUnmarshalDiscarded checks the values the target discards (a member
// with no field, elements past a fixed array) to the bottom: a nested
// BigInt, a cycle or a throwing toJSON is the round trip's error, and a
// getter runs as many times as AppendJSON runs it.
func TestUnmarshalDiscarded(t *testing.T) {
	mod, err := moejs.Compile("d.js", `
export function w() { return {Known: 1, unknown: {deep: {deeper: 1n}}}; }
export function x() { return [1, [2, {a: 1n}]]; }
export function cyc() { const o = {i: 1}; o.self = o; return {Known: 2, unknown: o}; }
export function boom() { return {Known: 3, A2: [1, {toJSON() { throw new Error("boom"); }}, 3]}; }
let n = 0;
export function getter() { return {Known: 4, unknown: {get g() { n++; return 1; }}}; }
export function count() { return n; }
export function fn() { return {Known: 5, f: Object.assign(() => 1, {toJSON() { return "F"; }}), Fn: Object.assign(() => 1, {toJSON() { return 7; }})}; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	type K struct {
		Known int
		Fn    int
	}
	for _, c := range []struct {
		hook string
		err  string
	}{
		{"w", "TypeError: Do not know how to serialize a BigInt"},
		{"cyc", "TypeError: Converting circular structure to JSON"},
		{"boom", "Error: boom"},
		{"getter", ""},
		{"fn", ""},
	} {
		v, err := rt.Call(mustHook(t, mod, c.hook))
		require.NoError(t, err)
		var k K
		err = rt.Unmarshal(v, &k)
		if c.err != "" {
			require.ErrorContains(t, err, c.err, c.hook)
			require.Zero(t, k, "%s: nothing written when AppendJSON fails", c.hook)
			continue
		}
		require.NoError(t, err, c.hook)
	}
	cnt, err := rt.Call(mustHook(t, mod, "count"))
	require.NoError(t, err)
	require.Equal(t, 1.0, cnt.AsNumber(), "the getter ran once, in AppendJSON")
	v, err := rt.Call(mustHook(t, mod, "fn"))
	require.NoError(t, err)
	var k K
	require.NoError(t, rt.Unmarshal(v, &k))
	require.Equal(t, K{Known: 5, Fn: 7}, k, "a function's toJSON replaces it")
	v, err = rt.Call(mustHook(t, mod, "x"))
	require.NoError(t, err)
	var arr [1]int
	require.ErrorContains(t, rt.Unmarshal(v, &arr), "Do not know how to serialize a BigInt")
	require.Zero(t, arr)

	// A host argument whose Go value AppendJSON cannot write (a *big.Int, a
	// cyclic map of JSON's types, an invalid json.Number) is checked before
	// anything is written too: the round trip's error, the target as it was,
	// whether the target keeps the argument, discards it or cannot hold it.
	hmod, err := moejs.Compile("h.js", `export function kept(h) { return {Known: 6, Body: h}; }
export function discarded(h) { return {Known: 6, unknown: h}; }
export function twice(h) { return {Known: 6, Body: h, Again: [h]}; }
export function mixed(h) { return {unknown: h, Known: 6, Body: h, Again: [h, 1], Req: {Body: h}}; }`)
	require.NoError(t, err)
	hrt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, hrt.Load(hmod))
	cyclic := map[string]any{"k": "v"}
	cyclic["self"] = cyclic
	type withAny struct {
		Known int
		Body  any
		Again []any
	}
	targets := []func() any{
		func() any { return &withAny{Known: 1} },
		func() any { return &struct{ Known int }{Known: 1} },
		func() any { return &map[string]any{"keep": 1} },
		func() any { var a any; return &a },
		func() any { return &struct{ Req struct{ Body any } }{} }, // deeper than jsonHoldsAny looks
	}
	for _, h := range []any{map[string]any{"n": big.NewInt(1)}, cyclic, map[string]any{"n": json.Number("abc")}, map[string]any{"ok": []any{1.5, "s"}}, map[string]any{"f32": []any{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))}}} {
		for _, hook := range []string{"kept", "discarded", "twice", "mixed"} {
			for _, mk := range targets {
				call := func() moejs.Value {
					arg, err := hrt.FromGo(h)
					require.NoError(t, err)
					v, err := hrt.Call(mustHook(t, hmod, hook), arg)
					require.NoError(t, err)
					return v
				}
				got, want := mk(), mk()
				gerr := hrt.Unmarshal(call(), got)
				data, werr := hrt.AppendJSON(nil, call())
				if werr == nil {
					werr = json.Unmarshal(data, want)
				}
				require.Equal(t, umErr(werr), umErr(gerr), "%s %T", hook, got)
				require.Equal(t, want, got, "%s %T", hook, got)
			}
		}
	}
	// Each place the argument goes gets a copy of its own.
	arg, err := hrt.FromGo(map[string]any{"ok": []any{1.5, "s"}})
	require.NoError(t, err)
	hv, err := hrt.Call(mustHook(t, hmod, "twice"), arg)
	require.NoError(t, err)
	var hb withAny
	require.NoError(t, hrt.Unmarshal(hv, &hb))
	hb.Body.(map[string]any)["ok"] = nil
	require.Equal(t, []any{map[string]any{"ok": []any{1.5, "s"}}}, hb.Again)

	// A toJSON on Function.prototype (a mutable realm) replaces a function.
	mrt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: true})
	mmod, err := moejs.Compile("m.js", `export function v() { Function.prototype.toJSON = function () { return "F"; }; return {a: () => 1, b: [() => 2]}; }`)
	require.NoError(t, err)
	require.NoError(t, mrt.Load(mmod))
	mv, err := mrt.Call(mustHook(t, mmod, "v"))
	require.NoError(t, err)
	var got any
	require.NoError(t, mrt.Unmarshal(mv, &got))
	require.Equal(t, map[string]any{"a": "F", "b": []any{"F"}}, got)
}

package engine

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// togoRoundTrip is ToGoInto's reference: json.Unmarshal of json.Marshal of
// ToGoStrict's result, with the stage that failed.
func togoRoundTrip(r *Realm, v Value, target any) (stage string, err error) {
	g, err := r.ToGoStrict(v)
	if err != nil {
		return "ToGo", err
	}
	data, err := json.Marshal(g)
	if err != nil {
		return "Marshal", err
	}
	return "", json.Unmarshal(data, target)
}

// togoLikeRoot is the root API's ToGoInto: the walk, then the round trip
// when it did not complete.
func togoLikeRoot(r *Realm, v Value, target any) (complete bool, err error) {
	if r.ToGoInto(v, target) {
		return true, nil
	}
	_, err = togoRoundTrip(r, v, target)
	return false, err
}

type tgInner struct {
	I, J int
}

type tgTarget struct {
	Model    string            `json:"model"`
	N        int               `json:"n"`
	Ratio    float64           `json:"ratio"`
	Headers  map[string]string `json:"headers"`
	Body     any               `json:"body"`
	Inner    *tgInner          `json:"inner"`
	List     []tgInner         `json:"list"`
	Tags     []string          `json:"tags"`
	Untagged string
}

// TestToGoIntoCompletes checks that the common shapes go in without the
// round trip, with its result: undefined as null, -0 kept in an interface,
// a class instance and a null-prototype object as plain objects, and an
// argument's Go value, unmodified or modified.
func TestToGoIntoCompletes(t *testing.T) {
	r := NewRealm()
	for _, src := range []string{
		`({model: "m", n: 1, ratio: 0.5, headers: {a: "b"}, body: {x: [1, "a", null, true, -0, 1e300]}, inner: {I: 1}, list: [{J: 2}], tags: ["t"], Untagged: "u"})`,
		`({model: undefined, body: undefined, inner: undefined, list: [undefined], tags: undefined})`,
		`({body: {a: undefined, b: -0, c: 2**53, d: 0.1}})`,
		`new (class { constructor() { this.model = "c"; this.n = 3 } })()`,
		`Object.assign(Object.create(null), {model: "p", body: Object.assign(Object.create(null), {k: 1})})`,
		`({body: HOST})`,
		`(() => { const h = HOST; h.n = 5; return {body: h} })()`,
		`(() => { const h = HOST; h.n = 5; return h })()`,
		`({Model: "fold", MODEL: "last"})`,
	} {
		host, err := r.FromGo(map[string]any{"model": "m", "n": 2, "messages": []any{map[string]any{"role": "user"}}, "nil": []any(nil), "f32": float32(0.1)})
		require.NoError(t, err)
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("HOST"), host))
		v := evalValue(t, r, src)
		for _, mk := range []func() any{func() any { return new(tgTarget) }, func() any { return new(any) }, func() any { return new(map[string]any) }} {
			got, want := mk(), mk()
			require.True(t, r.ToGoInto(v, got), "%s into %T", src, got)
			stage, err := togoRoundTrip(r, v, want)
			require.NoError(t, err, "%s (%s)", src, stage)
			require.Equal(t, tgDump(want), tgDump(got), "%s into %T", src, got)
		}
	}
	// An argument's Go value goes into an empty interface only: into a
	// struct or a map the round trip runs, as for Unmarshal.
	host, err := r.FromGo(map[string]any{"model": "m", "n": 2, "nil": []any(nil)})
	require.NoError(t, err)
	var anyGot, anyWant any
	require.True(t, r.ToGoInto(host, &anyGot))
	_, err = togoRoundTrip(r, host, &anyWant)
	require.NoError(t, err)
	require.Equal(t, tgDump(anyWant), tgDump(anyGot))
	var sGot, sWant tgTarget
	complete, err := togoLikeRoot(r, host, &sGot)
	require.NoError(t, err)
	require.False(t, complete)
	_, err = togoRoundTrip(r, host, &sWant)
	require.NoError(t, err)
	require.Equal(t, sWant, sGot)

	var neg any
	require.True(t, r.ToGoInto(evalValue(t, r, `-0`), &neg))
	require.True(t, math.Signbit(neg.(float64)))
}

// TestToGoIntoFieldOrder checks the members that go to one struct field,
// by case folding, in a struct of a few fields and one of 70: each is
// written in the round trip's order, sorted by key, whatever the object's
// order. Into a map each key is its own entry.
func TestToGoIntoFieldOrder(t *testing.T) {
	r := NewRealm()
	fields := make([]reflect.StructField, 70)
	for i := range fields {
		fields[i] = reflect.StructField{Name: "F" + strconv.Itoa(i), Type: reflect.TypeFor[tgInner]()}
	}
	wide := reflect.StructOf(fields)
	for _, src := range []string{
		`({a: {J: 2}, A: {I: 1}})`, `({A: {I: 1}, a: {J: 2}})`, `({a: 1, A: 2})`,
		`({f69: {J: 2}, F69: {I: 1}, F0: {I: 3}})`, `({F69: {I: 1}, f69: {I: 4, J: 2}})`, `({F1: {I: 1}, F2: {J: 2}})`,
	} {
		v := evalValue(t, r, src)
		for _, mk := range []func() any{
			func() any { return new(struct{ A tgInner }) },
			func() any { return reflect.New(wide).Interface() },
			func() any { return new(map[string]any) },
		} {
			got, want := mk(), mk()
			_, gerr := togoLikeRoot(r, v, got)
			_, werr := togoRoundTrip(r, v, want)
			require.Equal(t, werr == nil, gerr == nil, "%s into %T", src, got)
			require.Equal(t, tgDump(want), tgDump(got), "%s into %T", src, got)
		}
	}
}

// TestToGoIntoKeepsTarget checks that the target is as it was when ToGo or
// json.Marshal fails: a NaN, a throwing getter, a cycle. A BigInt is not
// one of them: json.Marshal writes the *big.Int ToGo gives as a number.
func TestToGoIntoKeepsTarget(t *testing.T) {
	r := NewRealm()
	for src, stage := range map[string]string{
		`({model: "x", body: [1, NaN]})`:                             "Marshal",
		`({model: "x", body: Infinity})`:                             "Marshal",
		`(() => { const o = {model: "x"}; o.body = o; return o })()`: "Marshal",
		`(() => { const o = {model: "x"}; Object.defineProperty(o, "g", {get() { throw new Error("g") }, enumerable: true}); return o })()`: "ToGo",
	} {
		v := evalValue(t, r, src)
		got := tgTarget{Model: "old", Tags: []string{"t"}}
		require.False(t, r.ToGoInto(v, &got), src)
		require.Equal(t, tgTarget{Model: "old", Tags: []string{"t"}}, got, src)
		var want tgTarget
		s, err := togoRoundTrip(r, v, &want)
		require.Error(t, err, src)
		require.Equal(t, stage, s, src)
	}
}

// TestToGoIntoNumbers checks the number texts json.Marshal writes for
// ToGo's int64 and float64 against the targets that read them: overflow,
// a float into an integer, float32 rounding, json.Number.
func TestToGoIntoNumbers(t *testing.T) {
	r := NewRealm()
	type nums struct {
		I8  int8
		I64 int64
		U   uint
		F32 float32
		F64 float64
		N   json.Number
		Any any
	}
	for _, x := range []string{`0`, `-0`, `1`, `-1`, `127`, `128`, `1.5`, `2**53+2`, `2**63`, `-(2**63)`, `2**64`, `1e-7`, `1e21`, `1e300`, `16777217`, `5e-324`, `3.4028235677973366e38`} {
		v := evalValue(t, r, `(x => ({I8: x, I64: x, U: x, F32: x, F64: x, N: x, Any: x}))(`+x+`)`)
		got, want := new(nums), new(nums)
		_, gerr := togoLikeRoot(r, v, got)
		_, werr := togoRoundTrip(r, v, want)
		require.Equal(t, errText(werr), errText(gerr), x)
		require.Equal(t, tgDump(want), tgDump(got), x)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestToGoIntoNumberStop checks a json.Number from a string: the walk stops
// at it, having written the members before it in sorted key order, and the
// round trip, run on the same target, gives its result and error. The
// older implementation of encoding/json ends at the error, Go 1.27's goes
// on with the other members.
func TestToGoIntoNumberStop(t *testing.T) {
	r := NewRealm()
	v := evalValue(t, r, `({z: 1, b: "x", a: 2, c: 3})`)
	var got, want map[string]json.Number
	require.False(t, r.ToGoInto(v, &got))
	require.Equal(t, map[string]json.Number{"a": "2"}, got)
	_, gerr := togoRoundTrip(r, v, &got)
	_, werr := togoRoundTrip(r, v, &want)
	require.Error(t, werr)
	require.Equal(t, werr.Error(), errText(gerr))
	require.Equal(t, want, got)
}

// TestToGoIntoIgnoresInterrupt checks that a pending interrupt is not
// observed, as ToGo does not observe it, and stays pending.
func TestToGoIntoIgnoresInterrupt(t *testing.T) {
	r := NewRealm()
	large := evalValue(t, r, `Array.from({length: 5000}, (_, i) => ({i}))`)
	r.Interrupt("pending")
	t.Cleanup(r.ClearInterrupt)
	var got []struct{ I int }
	require.True(t, r.ToGoInto(large, &got))
	require.Len(t, got, 5000)
	require.Equal(t, 4999, got[4999].I)
	var ie *InterruptedError
	require.ErrorAs(t, r.CheckInterrupt(), &ie)
}

func TestToGoIntoInvalidTargets(t *testing.T) {
	r := NewRealm()
	v := evalValue(t, r, `({a: 1})`)
	var m map[string]any
	for _, target := range []any{nil, m, (*map[string]any)(nil), tgTarget{}} {
		require.False(t, r.ToGoInto(v, target))
	}
	raw := new(json.RawMessage) // the text of the value, json.Marshal's
	require.True(t, r.ToGoInto(v, raw))
	require.Equal(t, `{"a":1}`, string(*raw))
}

// tgDump writes v down to its last pointer, -0 apart from 0, interfaces
// with their dynamic types, maps in key order.
func tgDump(v any) string {
	var b []byte
	return string(tgDumpValue(b, reflect.ValueOf(v)))
}

func tgDumpValue(b []byte, rv reflect.Value) []byte {
	if !rv.IsValid() {
		return append(b, "invalid"...)
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return append(b, "nil"...)
		}
		return tgDumpValue(append(b, '&'), rv.Elem())
	case reflect.Interface:
		if rv.IsNil() {
			return append(b, "nil-iface"...)
		}
		b = append(b, rv.Elem().Type().String()+"("...)
		return append(tgDumpValue(b, rv.Elem()), ')')
	case reflect.Map:
		if rv.IsNil() {
			return append(b, "nil-map"...)
		}
		keys := rv.MapKeys()
		ks := make([]string, len(keys))
		for i, k := range keys {
			ks[i] = k.String()
		}
		sort.Strings(ks)
		b = append(b, '{')
		for _, k := range ks {
			b = append(b, strconv.Quote(k)+":"...)
			b = append(tgDumpValue(b, rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key()))), ',')
		}
		return append(b, '}')
	case reflect.Slice:
		if rv.IsNil() {
			return append(b, "nil-slice"...)
		}
		fallthrough
	case reflect.Array:
		b = append(b, '[')
		for i := range rv.Len() {
			b = append(tgDumpValue(b, rv.Index(i)), ',')
		}
		return append(b, ']')
	case reflect.Struct:
		b = append(b, '{')
		for i := range rv.NumField() {
			b = append(b, rv.Type().Field(i).Name+":"...)
			b = append(tgDumpValue(b, rv.Field(i)), ',')
		}
		return append(b, '}')
	case reflect.Bool:
		return strconv.AppendBool(b, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(b, rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(b, rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.AppendFloat(b, rv.Float(), 'g', -1, 64)
	case reflect.String:
		return strconv.AppendQuote(b, rv.String())
	}
	return append(b, rv.Kind().String()...)
}

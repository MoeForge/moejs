package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawJSONCorpus holds texts of objects and arrays the parser accepts and
// rejects: white space, every escape, numbers at the edges of the grammar,
// bytes that are not valid UTF-8 inside and outside strings, nesting at and
// past jsonMaxDepth.
func rawJSONCorpus() []string {
	deep := func(n int, open, close string) string {
		return strings.Repeat(open, n) + strings.Repeat(close, n)
	}
	return []string{
		`{}`, `[]`, ` { } `, "\t[\n]\r", `{"a":1}`, `[1,2,3]`, ` [ 1 , [ ] , { } , "s" ] `,
		`{"a":{"b":[{"c":null}]},"d":true,"e":false}`,
		`{"a":1,"a":2}`, `{"":0}`, `{"1":1,"a":2,"0":0}`, `{"4294967294":1,"4294967295":2}`,
		`["\"\\\/\b\f\n\r\tAé😀𐀀\udfff"]`,
		`["é😀", "  ", "<>&"]`, "[\"a\xffb\xc3\"]", "[\"\xed\xa0\x80\"]",
		`[0, -0, 1.5, -1.5e10, 1E+2, 1e-2, 1e21, 5e-324, 1e400, -1e400, 123456789012345678901234567890]`,
		`[0.1, 9007199254740993, -9007199254740993, 2.2250738585072014e-308]`,
		deep(jsonMaxDepth, "[", "]"), deep(jsonMaxDepth+1, "[", "]"),
		strings.Repeat(`{"a":`, jsonMaxDepth) + "1" + strings.Repeat("}", jsonMaxDepth),
		strings.Repeat(`{"a":`, jsonMaxDepth+1) + "1" + strings.Repeat("}", jsonMaxDepth+1),
		// rejected
		`{`, `[`, `{"a"}`, `{"a":}`, `{a:1}`, `{"a":1,}`, `[1,]`, `[,1]`, `[1 2]`, `{"a":1}}`, `[]]`, `[] x`,
		`[01]`, `[1.]`, `[.5]`, `[-]`, `[1e]`, `[1e+]`, `[+1]`, `[0x10]`, `[NaN]`, `[Infinity]`, `[tru]`, `[nul]`, `[True]`,
		`["a]`, `["\x"]`, `["\u12"]`, `["\u12g4"]`, "[\"\x01\"]", "[\"\t\"]", `["\`, `{"a":1 "b":2}`, `{"a" 1}`,
		"[1]\xff", "[\xff]", "{\"a\":1,\xc3\xa9:2}", "\v[]", "[ ]", `{"a":[}`, `[{]`, `[1}`, `{"a":1]`,
	}
}

func TestJSONCheck(t *testing.T) {
	r := NewRealm()
	for _, s := range rawJSONCorpus() {
		checkJSONCheck(t, r, s)
	}
}

// checkJSONCheck compares jsonCheck with the parser on s.
func checkJSONCheck(t *testing.T, r *Realm, s string) {
	t.Helper()
	i := jsonSkipSpace(s, 0)
	if i >= len(s) || s[i] != '{' && s[i] != '[' {
		return
	}
	n, ok := jsonCheck(s, i)
	v, err := r.JSONParseGoString(s)
	name := s
	if len(name) > 40 {
		name = name[:40] + "..."
	}
	require.Equal(t, err == nil, ok, "%q: parse error %v", name, err)
	if !ok {
		return
	}
	o := v.AsObject()
	if o.class == ClassArray {
		assert.Equal(t, int(o.ArrayLength()), n, "%q", name)
	} else {
		assert.Equal(t, len(o.OwnEnumerableStringKeys()) == 0, n == 0, "%q", name)
	}
}

// FuzzJSONCheck: jsonCheck accepts exactly the texts the parser accepts, and
// counts an array's elements.
func FuzzJSONCheck(f *testing.F) {
	for _, s := range rawJSONCorpus() {
		if len(s) < 4096 {
			f.Add(s)
		}
	}
	r := NewRealm()
	f.Fuzz(func(t *testing.T, s string) {
		checkJSONCheck(t, r, s)
	})
}

// rawify replaces the containers among the values of m by their JSON text
// as json.RawMessage.
func rawify(t *testing.T, m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch v.(type) {
		case string, float64, bool, nil, int, int64, uint8, float32:
			out[k] = v
		default:
			b, err := json.Marshal(v)
			require.NoError(t, err)
			out[k] = json.RawMessage(b)
		}
	}
	return out
}

// TestFromGoRawJSONOperations applies every operation of hostLazyOps to the
// lazy sample given as JSON text, as one json.RawMessage and as a map of
// them, and to JSON.parse of the same text, and compares the results and
// the state of the argument afterwards.
func TestFromGoRawJSONOperations(t *testing.T) {
	src := `
export function run(op, x) {
	let res;
	try {
		res = describe(op(x));
	} catch (e) {
		res = "throws " + e.name + ": " + e.message;
	}
	return res + " | " + describe(x);
}
export const ops = [
` + strings.Join(hostLazyOps, ",\n") + `
];
`
	text, err := json.MarshalIndent(hostLazySample(), " ", "\t")
	require.NoError(t, err)
	for _, realm := range hostLazyRealms {
		t.Run(realm.name, func(t *testing.T) {
			f := newHostLazyFixture(t, src, realm.opts)
			ops := f.fn("ops").AsObject()
			for i, opText := range hostLazyOps {
				op, err := ops.GetIndex(f.r, uint32(i))
				require.NoError(t, err)
				for _, in := range []any{json.RawMessage(text), rawify(t, hostLazySample())} {
					lazy := mustFromGo(t, f.r, in)
					parsed, err := f.r.JSONParseGoString(string(text))
					require.NoError(t, err)
					for round := range 2 {
						want := f.callValues("run", op, parsed)
						got := f.callValues("run", op, lazy)
						require.Equal(t, want, got, "op %s, %T, round %d", opText, in, round)
					}
				}
			}
		})
	}
}

// TestFromGoRawJSON: a json.RawMessage converts to JSON.parse of its text,
// parsed on first touch when it is an object or an array.
func TestFromGoRawJSON(t *testing.T) {
	r := NewRealm()

	// Texts of every kind give what JSON.parse gives, at the top level and
	// inside each container type.
	for _, s := range append(rawJSONCorpus(), `"str"`, ` 12 `, `-0`, `null`, `true`, `"\ud800"`, "\"a\xffb\"", `[1] `, `{"z":1,"a":[2]}`) {
		want, wantErr := r.JSONParseGoString(s)
		check := func(got Value, err error, where string) {
			t.Helper()
			if wantErr != nil {
				require.Error(t, err, "%s %q", where, s)
				assert.Equal(t, wantErr.Error(), err.Error(), "%s %q", where, s)
				return
			}
			require.NoError(t, err, "%s %q", where, s)
			assert.Equal(t, stringifyGo(t, r, want), stringifyGo(t, r, got), "%s %q", where, s)
		}
		raw := json.RawMessage(s)
		got, err := r.FromGo(raw)
		check(got, err, "top")
		for _, c := range []struct {
			in   any
			read func(Value) (Value, error)
		}{
			{map[string]any{"k": raw, "other": 1.0}, func(v Value) (Value, error) { return v.AsObject().GetProp(r, key(r, "k")) }},
			{[]any{1.0, raw}, func(v Value) (Value, error) { return v.AsObject().GetIndex(r, 1) }},
			{[]any{[]any{map[string]any{"k": raw}}}, func(v Value) (Value, error) {
				a, _ := v.AsObject().GetIndex(r, 0)
				m, _ := a.AsObject().GetIndex(r, 0)
				return m.AsObject().GetProp(r, key(r, "k"))
			}},
		} {
			got, err := c.read(mustFromGo(t, r, c.in))
			check(got, err, fmt.Sprintf("in %T", c.in))
		}
		r.ReleaseCallData()
	}

	// nil is null, as json.Marshal writes it; empty text is a SyntaxError.
	v := mustFromGo(t, r, json.RawMessage(nil))
	assert.True(t, v.IsNull())
	_, err := r.FromGo(json.RawMessage{})
	var exc *Exception
	require.True(t, errors.As(err, &exc))
	assert.Equal(t, "SyntaxError: Unexpected end of JSON input", err.Error())
	in := map[string]any{"bad": json.RawMessage(`{"a":}`), "ok": json.RawMessage(`[1]`)}
	v = mustFromGo(t, r, in)
	_, err = v.AsObject().GetProp(r, key(r, "bad"))
	assert.EqualError(t, err, "SyntaxError: Unexpected token } in JSON at position 5")
	ok, err := v.AsObject().GetProp(r, key(r, "ok"))
	require.NoError(t, err)
	assert.Equal(t, "[1]", stringifyGo(t, r, ok))

	// Empty containers are ordinary objects, never placeholders.
	for _, s := range []string{"{}", " [ ] "} {
		v = mustFromGo(t, r, json.RawMessage(s))
		assert.Zero(t, v.AsObject().flags&flagHostNode, s)
	}
}

// TestFromGoRawJSONLazy: an object or array text is parsed on first touch;
// until then the value exports the RawMessage itself, and an array has its
// length.
func TestFromGoRawJSONLazy(t *testing.T) {
	r := NewRealm()
	raw := json.RawMessage(`{"b": [1, 2, {"c": "x"}], "a": "y"}`)
	v := mustFromGo(t, r, raw)
	o := v.AsObject()
	assert.Equal(t, hostSentinelKey, o.shape.key, "a placeholder")
	g, ok := o.HostValue()
	require.True(t, ok)
	assert.Equal(t, raw, g)
	assert.NotSame(t, &raw[0], &g.(json.RawMessage)[0], "a copy, not the host's RawMessage")
	g.(json.RawMessage)[2] = 'X' // the host may change the copy it got
	assert.Equal(t, raw, r.ToGo(v), "a new copy each time")

	b, err := o.GetProp(r, key(r, "b"))
	require.NoError(t, err)
	assert.Equal(t, uint32(3), b.AsObject().ArrayLength())
	assert.Zero(t, o.flags&flagHostNode, "an ordinary object once materialized")
	_, ok = o.HostValue()
	assert.False(t, ok)
	assert.Equal(t, map[string]any{"b": []any{int64(1), int64(2), map[string]any{"c": "x"}}, "a": "y"}, r.ToGo(v))
	assert.Equal(t, []string{"b", "a"}, keyNames(o.OwnEnumerableStringKeys()), "the text's order")

	// The parse read a copy: the host may reuse the bytes afterwards.
	copy(raw, `{"b": [9, 9, {"c": "z"}], "a": "w"}`)
	assert.Equal(t, `{"b":[1,2,{"c":"x"}],"a":"y"}`, stringifyGo(t, r, v))

	// An array placeholder has its length before it is materialized.
	arr := json.RawMessage(` [ [1, 2], {"a": [3]}, "s", 4 ] `)
	v = mustFromGo(t, r, map[string]any{"list": arr})
	list, err := v.AsObject().GetProp(r, key(r, "list"))
	require.NoError(t, err)
	lo := list.AsObject()
	assert.Equal(t, uint32(4), lo.ArrayLength())
	assert.Equal(t, hostSentinelKey, lo.shape.key, "still a placeholder")
	g, ok = lo.HostValue()
	require.True(t, ok)
	assert.Equal(t, arr, g)
	assert.NotSame(t, &arr[0], &g.(json.RawMessage)[0])
	assert.Equal(t, `[[1,2],{"a":[3]},"s",4]`, stringifyGo(t, r, list))
	assert.Equal(t, []any{[]any{int64(1), int64(2)}, map[string]any{"a": []any{int64(3)}}, "s", int64(4)}, r.ToGo(list))

	// A pending interrupt does not stop a materialization.
	v = mustFromGo(t, r, json.RawMessage(`[`+strings.Repeat(`1,`, 9999)+`1]`))
	r.Interrupt("stop")
	assert.Len(t, v.AsObject().Elements(), 10000)
	r.ClearInterrupt()

	// Wide objects, index keys and a prototype changed before the parse.
	var sb strings.Builder
	sb.WriteString(`{"1": "one", "4294967294": "big"`)
	for i := range 100 {
		fmt.Fprintf(&sb, `, "k%d": %d`, i, i)
	}
	sb.WriteString(`}`)
	want, err := r.JSONParseGoString(sb.String())
	require.NoError(t, err)
	v = mustFromGo(t, r, json.RawMessage(sb.String()))
	proto := r.NewObject()
	require.True(t, v.AsObject().SetPrototypeOf(r, proto))
	assert.Equal(t, stringifyGo(t, r, want), stringifyGo(t, r, v))
	assert.Same(t, proto, v.AsObject().Proto())
	v = mustFromGo(t, r, json.RawMessage(`{"x": 1, "y": [2]}`))
	require.True(t, v.AsObject().SetPrototypeOf(r, nil))
	x, err := v.AsObject().GetProp(r, key(r, "x"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(1), x)
	assert.Nil(t, v.AsObject().Proto())
}

// TestFromGoRawJSONCopied: FromGo converts a RawMessage from a copy of its
// bytes, so a host that changes them afterwards (a buffer reused for the next
// request) changes nothing JavaScript reads, whatever the new bytes are.
func TestFromGoRawJSONCopied(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct{ before, after string }{
		{`{"a": 1}`, `{"a": }`},
		{`{"a": 1}`, `[1,2,3]`},
		{`[1, 2]`, `[1, 2,3]`},
		{`[1, 2]`, `{"a":1}`},
		{`{"a": 1}`, `{"b": 2}`},
	} {
		raw := json.RawMessage(c.before)
		v := mustFromGo(t, r, raw)
		want, err := r.JSONParseGoString(c.before)
		require.NoError(t, err)
		copy(raw, c.after)
		assert.Equal(t, stringifyGo(t, r, want), stringifyGo(t, r, v), "%s -> %s", c.before, c.after)
		// Inside a container the copy is made when the container is read.
		inner := json.RawMessage(c.before)
		host := mustFromGo(t, r, map[string]any{"k": inner})
		member, err := host.AsObject().GetProp(r, key(r, "k"))
		require.NoError(t, err)
		copy(inner, c.after)
		assert.Equal(t, stringifyGo(t, r, want), stringifyGo(t, r, member), "in a map: %s -> %s", c.before, c.after)
	}
}

func TestFromGoRawJSONNonUTF8(t *testing.T) {
	r := NewRealm()
	s := "{\"k\xff\": \"v\xc3\", \"é\": \"\xed\xa0\x80\"}"
	require.False(t, utf8.ValidString(s))
	want, err := r.JSONParseGoString(s)
	require.NoError(t, err)
	v := mustFromGo(t, r, json.RawMessage(s))
	assert.Equal(t, stringifyGo(t, r, want), stringifyGo(t, r, v))
}

// TestUnmarshalRawTargets: a json.RawMessage target no longer sends the
// whole value through the round trip: the walk completes, writing the text
// the round trip writes into the target's own array when it fits.
func TestUnmarshalRawTargets(t *testing.T) {
	r := NewRealm()
	type result struct {
		TaskID   string                     `json:"taskId"`
		TaskData json.RawMessage            `json:"taskData"`
		State    *json.RawMessage           `json:"state"`
		Parts    map[string]json.RawMessage `json:"parts"`
		List     []json.RawMessage          `json:"list"`
	}
	v := evalValue(t, r, `({taskId: "t1", taskData: {b: "<é>", a: [1, -0, 1e21, undefined]}, state: null, parts: {x: {z: 1, y: 2}, n: null}, list: [1, "s", undefined, {k: [true]}]})`)
	for _, togo := range []bool{false, true} {
		var got result
		buf := make(json.RawMessage, 0, 128)
		got.TaskData = buf
		var complete bool
		if togo {
			complete = r.ToGoInto(v, &got)
		} else {
			var err error
			complete, err = r.Unmarshal(v, &got)
			require.NoError(t, err)
		}
		require.True(t, complete, "togo=%v", togo)
		assert.Same(t, &buf[:1][0], &got.TaskData[0], "the target's array, as RawMessage.UnmarshalJSON reuses it")
		assert.Nil(t, got.State)
		if togo {
			assert.Equal(t, `{"a":[1,-0,1e+21,null],"b":"\u003cé\u003e"}`, string(got.TaskData), "json.Marshal's escapes")
			assert.Equal(t, map[string]json.RawMessage{"x": json.RawMessage(`{"y":2,"z":1}`), "n": json.RawMessage("null")}, got.Parts)
			assert.Equal(t, []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"s"`), json.RawMessage("null"), json.RawMessage(`{"k":[true]}`)}, got.List)
		} else {
			assert.Equal(t, `{"b":"<é>","a":[1,0,1e+21,null]}`, string(got.TaskData))
			assert.Equal(t, map[string]json.RawMessage{"x": json.RawMessage(`{"z":1,"y":2}`), "n": json.RawMessage("null")}, got.Parts)
			assert.Equal(t, []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"s"`), json.RawMessage("null"), json.RawMessage(`{"k":[true]}`)}, got.List)
		}
	}

	// A RawMessage FromGo converted and JavaScript passed on untouched goes
	// into a RawMessage target with no parse: json.Marshal's compact text
	// for ToGoInto.
	state := json.RawMessage(` {"step": 2, "ids": [1, 2]} `)
	arg := mustFromGo(t, r, map[string]any{"state": state})
	st, err := arg.AsObject().GetProp(r, key(r, "state"))
	require.NoError(t, err)
	out := r.NewObject()
	out.SetProp(r, key(r, "state"), st)
	var got result
	require.True(t, r.ToGoInto(ObjectValue(out), &got))
	assert.Equal(t, `{"step":2,"ids":[1,2]}`, string(*got.State))
	assert.Equal(t, hostSentinelKey, st.AsObject().shape.key, "never parsed")
	complete, err := r.Unmarshal(ObjectValue(out), &got)
	require.NoError(t, err)
	require.True(t, complete)
	assert.Equal(t, `{"step":2,"ids":[1,2]}`, string(*got.State))

	// A value the round trip has to run code for still takes it.
	v = evalValue(t, r, `({taskData: {toJSON() { return 1 }}})`)
	complete, err = r.Unmarshal(v, &got)
	require.NoError(t, err)
	assert.False(t, complete)
}

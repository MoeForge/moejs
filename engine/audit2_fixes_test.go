package engine

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Array.prototype.sort without a comparator converts every element to a
// string before it compares any; the conversions of an array of primitives
// run no bytecode, so the keying loop checks for an interrupt itself.
func TestSortKeyingInterrupt(t *testing.T) {
	f, err := auditAModule(t, `export function run(ts, n) { const o = {toString: ts}; return Array(n).fill(o).sort() }`)
	require.NoError(t, err)
	r := f.r
	calls := 0
	ts := nativeFn(r, func(Value, []Value) (Value, error) {
		if calls++; calls == 1 {
			r.Interrupt("x")
		}
		return StringValue(asciiString("k")), nil
	})
	run, _ := f.env.GetBindingValue("run")
	_, err = r.Call(run, Undefined(), []Value{ts, IntValue(1 << 16)})
	r.ClearInterrupt()
	var ie *InterruptedError
	require.True(t, errors.As(err, &ie), "err=%v", err)
	require.LessOrEqual(t, calls, interruptStride)
}

// DropJobs discards the queue at the outermost boundary only, and works on
// a realm that never queued a job.
func TestDropJobs(t *testing.T) {
	r := NewRealm()
	r.DropJobs(nil)
	ran := 0
	fn := nativeFn(r, func(Value, []Value) (Value, error) { ran++; return Undefined(), nil })
	r.HoldJobs()
	r.enqueue(job{fn: fn.AsObject()})
	r.DropJobs(nil)
	require.Len(t, r.lazy.jobs.queue, 1, "a no-op inside a call")
	require.NoError(t, r.ReleaseJobs(nil))
	require.Equal(t, 1, ran)
	r.enqueue(job{fn: fn.AsObject()})
	r.DropJobs(nil)
	require.False(t, r.jobsPending)
	r.HoldJobs()
	require.NoError(t, r.ReleaseJobs(nil))
	require.Equal(t, 1, ran, "the dropped job does not run")
}

// IsForeign flags the functions and generators whose bytecode another realm
// created, through bound functions and proxy targets, and the callable
// proxies another realm created, whatever their target; natives, shared
// intrinsics, primitives, object proxies, the realm's own revoked proxies
// and module namespaces run anywhere.
func TestIsForeign(t *testing.T) {
	b, err := auditAModule(t, `export function f() {} export function* g() {} export async function* ag() {}
export const bf = f.bind(null), bb = bf.bind(null), it = g(), ait = ag(), nb = Math.max.bind(null), max = Math.max;
export const px = new Proxy(f, {}), ppx = new Proxy(px, {}), bpx = px.bind(null), gpx = new Proxy(it, {});
export const opx = new Proxy({f}, {}), mpx = new Proxy(Math.max, {});
const rv = Proxy.revocable(f, {}); rv.revoke();
export const rvk = rv.proxy;`)
	require.NoError(t, err)
	a, err := auditAModule(t, `export function f() {}
export const mpx = new Proxy(Math.max, {});
const rv = Proxy.revocable(f, {}); rv.revoke();
export const rvk = rv.proxy;`)
	require.NoError(t, err)
	for _, name := range []string{"f", "g", "ag", "bf", "bb", "it", "ait", "px", "ppx", "bpx", "gpx", "mpx", "rvk"} {
		v, _ := b.env.GetBindingValue(name)
		require.True(t, a.r.IsForeign(v), name)
		require.False(t, b.r.IsForeign(v), name)
	}
	for _, name := range []string{"nb", "max", "opx"} {
		v, _ := b.env.GetBindingValue(name)
		require.False(t, a.r.IsForeign(v), name)
	}
	for _, name := range []string{"mpx", "rvk"} {
		v, _ := a.env.GetBindingValue(name)
		require.False(t, a.r.IsForeign(v), name)
	}
	require.False(t, a.r.IsForeign(nativeFn(b.r, func(Value, []Value) (Value, error) { return Undefined(), nil })))
	require.False(t, a.r.IsForeign(IntValue(1)))
	px := &Object{class: ClassProxy}
	px.setPayload(&moduleNamespace{})
	require.False(t, a.r.IsForeign(ObjectValue(px)))
	own, _ := a.env.GetBindingValue("f")
	require.False(t, a.r.IsForeign(own))
}

// Two Go keys that are not valid UTF-8 can decode to one atom: the object
// has the key once, holding the entry of the later key in byte order (as
// JSON.parse keeps the last of duplicate names), on the shape path, the
// dictionary path and the key-by-key path of a map with an entry that fails
// to convert, and no shape with a shadowed entry reaches the shared tree.
func TestHostDuplicateKeys(t *testing.T) {
	const src = `export function show(m, k) {
	let forin = 0;
	for (const x in m) if (x === k) forin++;
	const out = [Object.keys(m).length, Reflect.ownKeys(m).length, JSON.stringify(m).split(JSON.stringify(k)).length - 1, forin, String(m[k]),
		Object.keys({...m}).length, Object.keys(Object.assign({}, m)).length, structuredClone(m)[k] === m[k]];
	out.push(delete m[k], k in m, Object.keys(m).length);
	return JSON.stringify(out);
}
export function get(m, k) { try { return Object.keys(m).length + ":" + String(m[k]); } catch (e) { return Object.keys(m).length + ":" + e.name; } }
export function reproto(m, k) { Object.setPrototypeOf(m, null); return m[k]; }`
	wide := func(extra map[string]string) map[string]string {
		m := maps.Clone(extra)
		for i := range 70 {
			m[fmt.Sprintf("k%02d", i)] = "v"
		}
		return m
	}
	cases := []struct {
		name string
		m    func() any
		key  string
		want string
	}{
		{"shape", func() any { return map[string]any{"\xff": 1, "\xfe": 2, "ok": 3} }, "�", `[2,2,1,1,"1",2,2,true,true,false,1]`},
		{"valid", func() any { return map[string]any{"�": 1, "\xff": 2} }, "�", `[1,1,1,1,"2",1,1,true,true,false,0]`},
		{"apart", func() any { return map[string]any{"a�": 1, "a\U0001F600": 2, "a\xff": 3} }, "a�", `[2,2,1,1,"3",2,2,true,true,false,1]`},
		{"strings", func() any { return map[string]string{"\xfe": "x", "\xff": "y"} }, "�", `[1,1,1,1,"y",1,1,true,true,false,0]`},
		{"lists", func() any { return map[string][]string{"\xfe": {"x"}, "\xff": {"y"}} }, "�", `[1,1,1,1,"y",1,1,false,true,false,0]`},
		{"dict", func() any { return wide(map[string]string{"\xfe": "x", "\xff": "y"}) }, "�", `[71,71,1,1,"y",71,71,true,true,false,70]`},
		{"failing", func() any { return map[string]any{"\xfe": make(chan int), "\xff": 2, "ok": 3} }, "�", `[2,2,1,1,"2",2,2,true,true,false,1]`},
	}
	for _, realm := range hostLazyRealms {
		t.Run(realm.name, func(t *testing.T) {
			f := evalModuleWith(t, src, realm.opts)
			show, _ := f.env.GetBindingValue("show")
			for _, c := range cases {
				for range 2 { // the second conversion takes the cached shape
					res, err := f.r.Call(show, Undefined(), []Value{mustFromGo(t, f.r, c.m()), StringValue(f.r.InternGoString(c.key))})
					require.NoError(t, err, c.name)
					require.Equal(t, c.want, f.r.ToGo(res), c.name)

					o := mustFromGo(t, f.r, c.m()).AsObject()
					o.materializeHost()
					checkUniqueKeys(t, o, c.name)
				}
			}
			get, _ := f.env.GetBindingValue("get")
			res, err := f.r.Call(get, Undefined(), []Value{mustFromGo(t, f.r, map[string]any{"\xfe": 1, "\xff": make(chan int)}), StringValue(f.r.InternGoString("�"))})
			require.NoError(t, err)
			require.Equal(t, "1:TypeError", f.r.ToGo(res))

			reproto, _ := f.env.GetBindingValue("reproto")
			m := mustFromGo(t, f.r, map[string]any{"\xfe": 1, "\xff": 2, "ok": 3})
			res, err = f.r.Call(reproto, Undefined(), []Value{m, StringValue(f.r.InternGoString("�"))})
			require.NoError(t, err)
			require.Equal(t, int64(2), f.r.ToGo(res))
			checkUniqueKeys(t, m.AsObject(), "reproto")
		})
	}
	if p := sharedHostShapes.Load(); p != nil {
		for _, e := range *p {
			for ; e != nil; e = e.next {
				for _, prop := range e.shape.Props() {
					require.False(t, prop.key.IsPrivate(), "%q", e.keys)
				}
			}
		}
	}
}

// checkUniqueKeys fails when o has a named key twice or a shape with a
// shadowed entry in the shared tree.
func checkUniqueKeys(t *testing.T, o *Object, name string) {
	t.Helper()
	seen := map[PropertyKey]bool{}
	if o.flags&flagDict != 0 {
		for _, e := range o.dictTable().entries {
			if e.live() {
				require.False(t, seen[e.key], "%s: %s twice", name, e.key.GoString())
				seen[e.key] = true
			}
		}
		require.Equal(t, len(seen), o.dictTable().namedCount(), name)
		return
	}
	shadowed := false
	for _, p := range o.shape.Props() {
		require.False(t, seen[p.key], "%s: %s twice", name, p.key.GoString())
		seen[p.key] = true
		shadowed = shadowed || p.key.IsPrivate()
	}
	require.False(t, shadowed && o.shape.shared, "%s: shadowed entry in the shared tree", name)
}

// A private name is realm-local: a shape that holds one stays out of the
// shared tree when its object moves to a shared prototype.
func TestPrivateNameNotPublished(t *testing.T) {
	f := evalModuleWith(t, `class B { constructor(o) { return o; } }
class C extends B { #x = 7; static x(o) { return o.#x; } }
const o = Object.create({});
new C(o);
Object.setPrototypeOf(o, Object.prototype);
export const obj = o, x = C.x(o);`, RealmOptions{SharedIntrinsics: true})
	obj, _ := f.env.GetBindingValue("obj")
	x, _ := f.env.GetBindingValue("x")
	require.Equal(t, int64(7), f.r.ToGo(x))
	require.False(t, obj.AsObject().shape.shared)
}

// structuredClone of a Go container that contains itself, an unbounded tree,
// stops at maxCloneDepth with a RangeError instead of growing until memory
// runs out; JavaScript nesting is not bounded, and a JavaScript cycle still
// clones as a cycle.
func TestStructuredCloneHostCycle(t *testing.T) {
	f := evalModuleWith(t, `export function clone(v) { try { structuredClone(v); return "cloned"; } catch (e) { return e.name; } }
export function deep(n) { let o = {}; for (let i = 0; i < n; i++) o = {o}; return structuredClone(o) !== o; }
export function cycle() { const o = {}; o.o = o; const c = structuredClone(o); return c.o === c && c !== o; }`, RealmOptions{})
	self := map[string]any{"name": "self"}
	self["me"] = self
	list := []any{"x", nil}
	list[1] = list
	clone, _ := f.env.GetBindingValue("clone")
	stop := time.AfterFunc(500*time.Millisecond, func() { f.r.Interrupt("timeout") })
	defer stop.Stop()
	for _, v := range []any{self, list} {
		res, err := f.r.Call(clone, Undefined(), []Value{mustFromGo(t, f.r, v)})
		require.NoError(t, err)
		require.Equal(t, "RangeError", f.r.ToGo(res))
	}
	deep, _ := f.env.GetBindingValue("deep")
	res, err := f.r.Call(deep, Undefined(), []Value{IntValue(2 * maxCloneDepth)})
	require.NoError(t, err)
	require.Equal(t, true, f.r.ToGo(res))
	cycle, _ := f.env.GetBindingValue("cycle")
	res, err = f.r.Call(cycle, Undefined(), nil)
	require.NoError(t, err)
	require.Equal(t, true, f.r.ToGo(res))
}

// JSON.parse builds on the realm's stack, which it leaves cleared, and a
// parsed object or array of up to eight entries is one allocation: a parse
// allocates its containers and strings only.
func TestJSONParseAllocs(t *testing.T) {
	r := NewRealm()
	parse := func(src string) error {
		_, err := r.JSONParse(FromGoString(src))
		return err
	}
	// Three objects, three arrays and the string "x".
	text := FromGoString(`{"a":[1,2,3],"b":{"c":"x","d":[{"e":1}]},"f":[]}`)
	v, err := r.JSONParse(text)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"a": []any{int64(1), int64(2), int64(3)}, "b": map[string]any{"c": "x", "d": []any{map[string]any{"e": int64(1)}}}, "f": []any{}}, r.ToGo(v))
	require.Equal(t, 7.0, testing.AllocsPerRun(50, func() { _, _ = r.JSONParse(text) }))
	for n := 1; n <= 10; n++ {
		a, err := r.JSONParse(FromGoString("[" + strings.Repeat("1,", n-1) + "1]"))
		require.NoError(t, err)
		require.Equal(t, uint32(n), a.AsObject().ArrayLength())
		require.Len(t, r.ToGo(a), n)
		var keys strings.Builder
		for i := range n {
			fmt.Fprintf(&keys, `,"k%d":%d`, i, i)
		}
		o, err := r.JSONParse(FromGoString("{" + keys.String()[1:] + "}"))
		require.NoError(t, err)
		require.Len(t, r.ToGo(o), n)
		require.Len(t, o.AsObject().slots, n)
	}
	// Nor does a successful one: the closed containers' entries, left in
	// place, are cleared once at the end.
	s := r.lazy.json
	zero := func(what string) {
		t.Helper()
		for i, v := range s.vals[:cap(s.vals)] {
			require.Equal(t, Value{}, v, "%s: value %d", what, i)
		}
		for i, k := range s.keys[:cap(s.keys)] {
			require.Equal(t, PropertyKey{}, k, "%s: key %d", what, i)
		}
	}
	require.NoError(t, parse(`{"a":[1,"s",{"x":"y","z":[true,{"q":"w"}]}],"b":{"c":"d","e":{"f":"g","h":["i"]}},"j":"k"}`))
	require.Empty(t, s.vals)
	require.Empty(t, s.keys)
	require.NotZero(t, cap(s.keys))
	zero("after a parse")
	// A failed parse leaves nothing on the kept stack.
	require.Error(t, parse(`{"a":{"b":[1,{"c":"x","d":`))
	require.Empty(t, s.vals)
	require.Empty(t, s.keys)
	for _, v := range s.vals[:cap(s.vals)] {
		require.Equal(t, Value{}, v)
	}
	for _, k := range s.keys[:cap(s.keys)] {
		require.Equal(t, PropertyKey{}, k)
	}
	// What a parse a Go panic stopped left in the arrays, DropJobs clears at
	// the outermost boundary.
	require.NotZero(t, cap(s.vals))
	for i := range s.vals[:cap(s.vals)] {
		s.vals[:cap(s.vals)][i] = IntValue(i + 1)
	}
	for i := range s.keys[:cap(s.keys)] {
		s.keys[:cap(s.keys)][i] = StringKey(AtomLength)
	}
	r.HoldJobs()
	r.DropJobs(nil)
	require.Equal(t, IntValue(1), s.vals[:cap(s.vals)][0], "a no-op inside a call")
	require.NoError(t, r.ReleaseJobs(nil))
	r.DropJobs(nil)
	require.Empty(t, s.vals)
	for _, v := range s.vals[:cap(s.vals)] {
		require.Equal(t, Value{}, v)
	}
	for _, k := range s.keys[:cap(s.keys)] {
		require.Equal(t, PropertyKey{}, k)
	}
	// A document past jsonStackMax entries leaves no room behind.
	require.NoError(t, parse("["+strings.Repeat("1,", jsonStackMax)+"1]"))
	require.Zero(t, cap(r.lazy.json.vals))
}

// JSON nesting is bounded at jsonMaxDepth on every path, with a RangeError,
// and the depth an error leaves behind is restored. Stringify and the
// reviver walk take walk units of the call depth, so a walk that starts
// another walk at its bottom (toJSON calling JSON.stringify or a host
// function that calls JSONStringify, a reviver calling JSON.parse) runs out
// of call depth at the second walk of 9,000 levels instead of holding
// hundreds of megabytes of Go stack. A walk's root takes jsonRootUnits, so
// walks at each one's last uncounted level stop at 228 (MaxCallDepth over
// the root's 16 calls and the two of toJSON and JSON.stringify), under 5 MB
// of Go stack. A hole is checked against the length limit when written, not
// up to 4096 holes later.
func TestJSONDepth(t *testing.T) {
	f := evalModuleWith(t, `function wrap(v, n) { for (let i = 0; i < n; i++) v = [v]; return v; }
export function deep(n) { return wrap(0, n); }
export function nestedStringify(n) {
	let count = 0;
	const leaf = { toJSON() { count++; return JSON.stringify(wrap(leaf, n)); } };
	try { JSON.stringify(leaf); return "ok"; } catch (e) { return e.name + "," + count; }
}
export function nestedHost(n, str) {
	let count = 0;
	const leaf = { toJSON() { count++; return str(wrap(leaf, n)); } };
	try { str(leaf); return "ok"; } catch (e) { return e.name + "," + count; }
}
export function nestedRevive(n) {
	let count = 0;
	const text = "[".repeat(n) + "0" + "]".repeat(n);
	function reviver(k, v) { if (v === 0) { count++; return JSON.parse(text, reviver); } return v; }
	try { JSON.parse(text, reviver); return "ok"; } catch (e) { return e.name + "," + count; }
}
export function holes(depth) {
	let n = 0;
	const inner = new Proxy(new Array(100000), { get(t, k) { if (k !== "length" && k !== "toJSON") n++; return Reflect.get(t, k); } });
	try { JSON.stringify(wrap(inner, depth), null, 10); return "ok"; } catch (e) { return e.name + "," + n; }
}`, RealmOptions{})
	r := f.r
	for _, unit := range []string{"[", `{"a":`} {
		closing := strings.Repeat("]", jsonMaxDepth)
		if unit != "[" {
			closing = strings.Repeat("}", jsonMaxDepth)
		}
		text := strings.Repeat(unit, jsonMaxDepth) + "0" + closing
		v, err := r.JSONParse(FromGoString(text))
		require.NoError(t, err)
		s, err := r.JSONStringify(v)
		require.NoError(t, err)
		require.Equal(t, text, s.GoString())
		_, err = r.JSONParse(FromGoString(unit + text))
		require.Equal(t, "RangeError: JSON nesting is too deep (more than 10000 levels)", errMessage(t, err))
	}
	deep, _ := f.env.GetBindingValue("deep")
	v, err := r.Call(deep, Undefined(), []Value{IntValue(jsonMaxDepth + 1)})
	require.NoError(t, err)
	_, err = r.JSONStringify(v)
	require.Equal(t, "RangeError: JSON nesting is too deep (more than 10000 levels)", errMessage(t, err))
	require.Zero(t, r.CallDepth())

	call := func(name string, n int) string {
		fn, _ := f.env.GetBindingValue(name)
		res, err := r.Call(fn, Undefined(), []Value{IntValue(n)})
		require.NoError(t, err)
		require.Zero(t, r.CallDepth())
		return res.AsString().GoString()
	}
	require.Equal(t, "RangeError,2", call("nestedStringify", 9000))
	require.Equal(t, "RangeError,1", call("nestedRevive", 9000))
	require.Equal(t, "RangeError,228", call("nestedStringify", jsonScanDepth-1))
	require.Equal(t, "RangeError,227", call("nestedRevive", jsonCallLevels-1))
	str := nativeFn(r, func(_ Value, args []Value) (Value, error) {
		s, err := r.JSONStringify(args[0])
		if err != nil {
			return Undefined(), err
		}
		return StringValue(s), nil
	})
	nestedHost, _ := f.env.GetBindingValue("nestedHost")
	for n, want := range map[int]string{9000: "RangeError,2", jsonScanDepth - 1: "RangeError,228"} {
		res, err := r.Call(nestedHost, Undefined(), []Value{IntValue(n), str})
		require.NoError(t, err)
		require.Zero(t, r.CallDepth())
		require.Equal(t, want, res.AsString().GoString(), "host, %d levels", n)
	}

	lowerMaxStringLength(t, 100000)
	// Each hole writes a line indented 10*depth bytes: about 100 fit.
	res := call("holes", 100)
	require.True(t, strings.HasPrefix(res, "RangeError,"), res)
	n, err := strconv.Atoi(res[len("RangeError,"):])
	require.NoError(t, err)
	require.Less(t, n, 200)
}

// TypedArrayElements refuses elements that are not aligned for their type,
// which only a buffer over host bytes can give; a byte view is always
// aligned.
func TestTypedArrayElementsAlignment(t *testing.T) {
	f := evalModule(t, `export function view(b, kind, off) { return new globalThis[kind](b, off); }`)
	view, _ := f.env.GetBindingValue("view")
	raw := make([]byte, 33) // a size class of 8-byte alignment
	for _, c := range []struct {
		from, off int
		kind      string
		ok        bool
	}{
		{0, 0, "Float64Array", true},
		{1, 0, "Float64Array", false},
		{4, 0, "Float64Array", false},
		{8, 0, "Float64Array", true},
		{1, 0, "Uint16Array", false},
		{2, 0, "Uint16Array", true},
		{2, 0, "Int32Array", false},
		{4, 4, "Int32Array", true},
		{1, 8, "BigUint64Array", false},
		{0, 8, "BigUint64Array", true},
		{1, 0, "Uint8Array", true},
		{3, 5, "Int8Array", true},
	} {
		buf, err := f.r.NewArrayBuffer(raw[c.from : c.from+24])
		require.NoError(t, err)
		v, err := f.r.Call(view, Undefined(), []Value{ObjectValue(buf), StringValue(FromGoString(c.kind)), IntValue(c.off)})
		require.NoError(t, err)
		elems, ok := v.AsObject().TypedArrayElements()
		require.Equal(t, c.ok, ok, "%s at raw[%d+%d]", c.kind, c.from, c.off)
		require.Equal(t, c.ok, elems != nil, "%s at raw[%d+%d]", c.kind, c.from, c.off)
	}
}

// Messages name a typed array by its constructor, as
// Object.prototype.toString does, not by its internal class.
func TestTypedArrayDisplayString(t *testing.T) {
	f := evalModule(t, `export const ta = new Uint8Array(1);
	export function transfer() {
		try { structuredClone(1, {transfer: [new Float32Array(3)]}); } catch (e) { return e.message; }
	}`)
	require.Equal(t, "DataCloneError: [object Float32Array] could not be transferred", f.call("transfer"))
	ta, _ := f.env.GetBindingValue("ta")
	require.Equal(t, "[object Uint8Array]", f.r.DisplayString(ta))
}

// An ownKeys trap result of a huge length fails at its first missing
// element, as the specification says, before any length limit applies.
func TestProxyOwnKeysHugeLength(t *testing.T) {
	f := evalModule(t, `export function keys(n) {
		try { return Reflect.ownKeys(new Proxy({}, {ownKeys() { return {length: n, 0: "a"}; }})).length; }
		catch (e) { return e.name + ": " + e.message; }
	}`)
	require.EqualValues(t, 1, f.call("keys", 1))
	require.Equal(t, "TypeError: undefined is not a valid property name", f.call("keys", 2))
	require.Equal(t, "TypeError: undefined is not a valid property name", f.call("keys", 1<<24+1))
}

// Object.assign onto a proxy whose trap returns false reports the trap, as
// a strict assignment to the proxy does.
func TestProxyFalsishAssignMessage(t *testing.T) {
	f := evalModule(t, `export function assign(trap) {
		const p = new Proxy({}, {[trap]() { return false; }});
		try { Object.assign(p, {x: 1}); } catch (e) { return e.message; }
	}
	export function strict() {
		"use strict";
		const p = new Proxy({}, {set() { return false; }});
		try { p.x = 1; } catch (e) { return e.message; }
	}`)
	want := "'set' on proxy: trap returned falsish for property 'x'"
	require.Equal(t, want, f.call("strict"))
	require.Equal(t, want, f.call("assign", "set"))
	require.Equal(t, want, f.call("assign", "defineProperty"))
}

package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of the lazy host conversion (hostlazy.go). The reference is the
// eager conversion (hostconv_eager_test.go): every operation on
// a lazily converted value must be indistinguishable from the same operation
// on the eagerly converted one.

// hostLazyDescribe renders a value and everything reachable from it: own
// keys in order with their attributes and values, prototypes, extensibility,
// array-ness and length. Two values render the same exactly when no
// property-level operation tells them apart.
const hostLazyDescribe = `
function describe(v, seen = [], depth = 0) {
	if (v === null) return "null";
	switch (typeof v) {
	case "undefined": return "undefined";
	case "string": return JSON.stringify(v);
	case "number": return v === 0 && 1 / v < 0 ? "-0" : String(v);
	case "boolean": return String(v);
	case "function": return "function " + v.name;
	}
	const i = seen.indexOf(v);
	if (i >= 0) return "#" + i;
	seen.push(v);
	if (depth > 12) return "...";
	const p = Object.getPrototypeOf(v);
	let s = p === Object.prototype ? "O" : p === Array.prototype ? "A" : p === null ? "N" : "P";
	if (Array.isArray(v)) s += "[" + v.length + "]";
	if (!Object.isExtensible(v)) s += "!x";
	if (Object.isFrozen(v)) s += "!f";
	s += "{";
	for (const k of Object.getOwnPropertyNames(v)) {
		const d = Object.getOwnPropertyDescriptor(v, k);
		s += k + ":" + (d.enumerable ? "e" : "") + (d.configurable ? "c" : "");
		s += "value" in d ? (d.writable ? "w" : "") + "=" + describe(d.value, seen, depth + 1) : "=accessor";
		s += ",";
	}
	return s + "}";
}
`

// hostLazyOps are the operations TestHostLazyOperations applies to a
// converted hostLazySample: every kind of read, write and reflection a plugin
// can apply to an argument.
var hostLazyOps = []string{
	// reads
	`x => x.model`,
	`x => x.missing`,
	`x => x[0] + x["1"] + x[10] + x["01"] + x[4294967295]`,
	`x => x.messages[0].role + x.messages[0].content`,
	`x => x.messages[0] === x.messages[0] && x.headers === x.headers`,
	`x => x.query.q.concat(x.query.e)`,
	`x => x.uni.length + x.uni.charCodeAt(1) + x.uni.codePointAt(6)`,
	`x => [typeof x.int, x.int, x.i64, x.u8, x.f32, x.n * 2]`,
	`x => x.model === x.upstreamModel`,
	`x => x.__proto__ === Object.prototype`,
	`x => Object.create(x).model`,
	`x => ({ __proto__: x.headers })["X-A"]`,
	`x => x["n"] + x.list[1] + x.list["1"] + x.list[1.0]`,
	`x => Object.create(Object.create(x.headers))["X-B"]`,
	// keys, enumeration, copies
	`x => Object.keys(x)`,
	`x => Object.values(x.headers)`,
	`x => Object.entries(x)`,
	`x => Object.entries(x.list)`,
	`x => Object.getOwnPropertyNames(x.messages)`,
	`x => Object.getOwnPropertyNames(x.list)`,
	`x => Object.assign({}, x)`,
	`x => ({ ...x })`,
	`x => { const { model, n, ...rest } = x; return rest; }`,
	`x => Object.fromEntries(Object.entries(x.headers))`,
	`x => { const ks = []; for (const k in x) ks.push(k); return ks; }`,
	`x => { const ks = []; for (const k in x.messages) ks.push(k); return ks; }`,
	`x => { const ks = []; for (const k in Object.create(x.headers)) ks.push(k); return ks; }`,
	`x => JSON.stringify(x)`,
	`x => JSON.stringify(x, null, 1)`,
	`x => JSON.stringify(x, ["model", "n", "headers", "X-A"])`,
	`x => JSON.stringify(x, (k, v) => typeof v === "number" ? v + 1 : v)`,
	`x => JSON.stringify(x.maps) + JSON.stringify(x.list)`,
	`x => String(x.headers) + x.list + Object.prototype.toString.call(x.list)`,
	`x => Object.entries(x.query).map(([k, v]) => k + v.length)`,
	// has / delete / descriptors
	`x => ["model" in x, "missing" in x, 0 in x, 2 in x.list, 5 in x.list, "length" in x.list]`,
	`x => [x.hasOwnProperty("n"), Object.hasOwn(x, "zz"), Object.hasOwn(x.list, 0), "toString" in x, x.propertyIsEnumerable("model")]`,
	`x => [delete x.model, delete x[0], delete x.missing, Object.keys(x)]`,
	`x => [delete x.list[0], x.list, 0 in x.list]`,
	`x => Object.getOwnPropertyDescriptor(x, "model")`,
	`x => Object.getOwnPropertyDescriptor(x.list, "length")`,
	`x => Object.getOwnPropertyDescriptors(x.headers)`,
	`x => Object.getOwnPropertyDescriptor(x.list, 1)`,
	// writes
	`x => { x.model = "changed"; x.extra = 1; x[7] = "idx"; return x; }`,
	`x => { x.headers["X-C"] = "3"; return Object.keys(x.headers); }`,
	`x => { x.headers = null; x.list[0] = x.list; return x; }`,
	`x => { "use strict"; x.n = 5; return x.n; }`,
	`x => { x.headers.__proto__ = null; return [Object.getPrototypeOf(x.headers), Object.keys(x.headers)]; }`,
	`x => Object.defineProperty(x, "model", { enumerable: false })`,
	`x => Object.defineProperty(x, "fresh", { value: 1 })`,
	`x => Object.defineProperty(x.list, 0, { value: "q" })`,
	`x => Object.defineProperty(x.list, "length", { writable: false }) && x.list.push("z")`,
	`x => Object.defineProperty(x.messages, "tag", { value: 1, enumerable: true })`,
	`x => { Object.defineProperty(x, "acc", { get() { return 1; }, enumerable: true }); return x.acc; }`,
	// integrity levels, prototypes
	`x => { Object.freeze(x); try { x.model = 1; return "no throw"; } catch (e) { return [e.name, Object.isFrozen(x), Object.isFrozen(x.headers)]; } }`,
	`x => { Object.seal(x.headers); return [Object.isSealed(x.headers), delete x.headers["X-A"]]; }`,
	`x => { Object.preventExtensions(x.list); return [Object.isExtensible(x.list), (() => { "use strict"; x.list[9] = 1; })(), x.list]; }`,
	`x => [Object.isFrozen(x.messages), Object.isSealed(x), Object.isFrozen(x.empty), Object.isExtensible(x.maps)]`,
	`x => [Object.getPrototypeOf(x) === Object.prototype, Object.getPrototypeOf(x.messages) === Array.prototype]`,
	`x => [x instanceof Object, x.messages instanceof Array, x instanceof Array, x.list.constructor === Array]`,
	`x => { Object.setPrototypeOf(x.headers, Array.prototype); return [typeof x.headers.join, Object.keys(x.headers)]; }`,
	`x => { Object.setPrototypeOf(x, { inherited: 1 }); return [x.inherited, Object.keys(x).length, x]; }`,
	`x => { Object.setPrototypeOf(x.list, null); return [Object.keys(x.list), x.list.length]; }`,
	// arrays
	`x => [Array.isArray(x.messages), Array.isArray(x.list), Array.isArray(x.maps), Array.isArray(x), Array.isArray(x.emptyList)]`,
	`x => x.messages.length + x.list.length + x.maps.length + x.emptyList.length`,
	`x => { x.list.length = 1; return x.list; }`,
	`x => { x.messages.length = 10; return x.messages; }`,
	`x => { x.messages[20] = 1; return x.messages.length; }`,
	`x => { x.list[5] = "gap"; return x.list; }`,
	`x => x.messages.map(v => typeof v)`,
	`x => x.messages.concat(x.list, x.maps)`,
	`x => [].concat(x.list, x.messages)`,
	`x => x.list.slice(1)`,
	`x => { x.list.push("z"); return x.list; }`,
	`x => { x.list.unshift("w"); return x.list; }`,
	`x => [x.messages.splice(1, 2), x.messages]`,
	`x => [x.list.pop(), x.list.shift(), x.list]`,
	`x => x.list.join("-") + x.list.indexOf("y") + x.messages.includes(null)`,
	`x => [...x.list, ...x.messages]`,
	`x => { const [a, b] = x.list; return a + b; }`,
	`x => Array.from(x.maps)`,
	`x => x.messages.reverse()`,
	`x => x.list.sort().reverse()`,
	`x => x.messages.flat(Infinity)`,
	`x => x.list.at(-1) + x.list.entries().next().value`,
	`x => { let s = ""; for (const v of x.list) s += v; return s; }`,
	`x => x.maps.filter(Boolean).map(m => Object.keys(m).length)`,
	`x => x.list.every(s => typeof s === "string") && x.messages.some(v => v === true)`,
	`x => x.messages.findLast(v => v !== null)`,
	`x => Array.prototype.slice.call(x.headers)`,
	`x => Array.prototype.map.call(x.list, s => s + s)`,
	`x => Array.prototype.join.call({ length: 2, 0: x.list, 1: x.maps[0] })`,
	`x => x.list == "x,y"`,
	`x => Object.keys(x.messages[6]).length + Object.keys(x.empty).length + Object.keys(x.maps[2]).length`,
	`x => x.maps[1] === null && x.nil === null`,
	`x => [x.list.toSorted(), x.messages.toReversed(), x.list.with(0, "w"), x.messages.toSpliced(1, 2, "n"), x.list]`,
	`x => [x.messages.copyWithin(0, 4), x.list.findLastIndex(s => s === "x")]`,
	// arrays with holes over a host placeholder on the prototype chain
	`x => { const a = [, "own1"]; Object.setPrototypeOf(a, x.list); return [a.shift(), a, Object.keys(a)]; }`,
	`x => { const a = [, "own1"]; Object.setPrototypeOf(a, x); return [Array.prototype.shift.call(a), a, Object.keys(a)]; }`,
	`x => { const a = [, "own1"]; Object.setPrototypeOf(a, x.list); a.unshift("u"); return [a, Object.keys(a)]; }`,
	`x => { const a = [, "own1", , "z"]; Object.setPrototypeOf(a, x.messages); return [a.splice(0, 2, "s"), a, Object.keys(a)]; }`,
	`x => { const a = [, "own1", , "z"]; Object.setPrototypeOf(a, x.messages); return [a.reverse(), Object.keys(a)]; }`,
	`x => { const a = [, "own1"]; Object.setPrototypeOf(a, x.list); const s = a.slice(0, 2); return [0 in s, s, [].concat(a), a.concat()]; }`,
	`x => { const a = [, "own1", , "z"]; Object.setPrototypeOf(a, x.messages); return [a.copyWithin(1, 0), Object.keys(a)]; }`,
	`x => { const a = [, "x", , "z"]; Object.setPrototypeOf(a, x.messages); return [a.toReversed(), a.toSorted(), a.toSpliced(0, 1), a.with(3, "w"), Object.keys(a)]; }`,
	`x => { const a = [, , "own2"], A = Array.prototype; Object.setPrototypeOf(a, Object.create(x)); return [A.reverse.call(a), A.slice.call(a), A.toReversed.call(a), Object.keys(a)]; }`,
	// Reflect, Object.is, legacy accessors
	`x => [Reflect.ownKeys(x), Reflect.ownKeys(x.list), Reflect.has(x, "model"), Reflect.get(x.headers, "X-A"), Reflect.getOwnPropertyDescriptor(x.list, 0)]`,
	`x => [Reflect.set(x, "model", 1), Reflect.deleteProperty(x.list, 0), Reflect.defineProperty(x.headers, "X-D", { value: 4 }), x]`,
	`x => [Reflect.preventExtensions(x.messages), Reflect.isExtensible(x.messages), Reflect.getPrototypeOf(x.list) === Array.prototype, Reflect.setPrototypeOf(x.maps, null)]`,
	`x => Reflect.apply(Array.prototype.join, x.list, ["+"])`,
	`x => [Object.is(x.headers, x.headers), Object.is(x.list[0], "x")]`,
	`x => [typeof x.__lookupGetter__("model"), x.__defineGetter__("g", () => 7), x.g, Object.keys(x)]`,
	// iteration, species, symbol protocols, structuredClone
	`x => [[...x.list], Array.from(x.messages), new Set(x.list).size, new Map(Object.entries(x.headers)).get("X-A")]`,
	`x => { const [a, ...b] = x.messages; const out = []; for (const [k, v] of Object.entries(x.headers)) out.push(k + v); return [a, b, out]; }`,
	`x => [x.list.concat(x.messages, x.query.q), x.list.map(s => s + 1), x.maps.slice(1), x.messages.flat()]`,
	`x => [String(x.headers), Object.prototype.toString.call(x.list), String(x.list), x.headers + "", Object.groupBy(x.list, s => s)]`,
	`x => [structuredClone(x), structuredClone(x.list), structuredClone(x.messages)]`,
	// class private names on host values
	`x => { class C { #p = 1; static has(o) { return #p in o; } static get(o) { return o.#p; } } try { return [C.has(x), C.has(x.list), C.get(x)]; } catch (e) { return [C.has(x), e.name]; } }`,
	`x => { class C { #m() { return 1; } static has(o) { return #m in o; } } return [C.has(x.messages), C.has(x.headers)]; }`,
	`x => { class B { constructor(o) { return o; } } class D extends B { #p = 1; constructor(o) { super(o); } static get(o) { return o.#p; } } new D(x.headers); new D(x.list); return [D.get(x.headers), D.get(x.list), Object.keys(x.headers), x.list]; }`,
}

// hostLazySample is an argument with every host container type, index-like
// keys, shared strings, typed numbers and empty and nil children; it returns
// a fresh value on every call.
func hostLazySample() map[string]any {
	model := "gpt-" + strconv.Itoa(4) // one Go string in two places
	return map[string]any{
		"model": model, "upstreamModel": model,
		"n": 2.0, "stream": false, "nil": nil,
		"b": "second", "a": "first", "1": "one", "0": "zero", "10": "ten", "01": "not an index", "4294967295": "named",
		"headers":   map[string]string{"X-B": "2", "X-A": "1"},
		"query":     map[string][]string{"q": {"a", "b"}, "e": {}},
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}, "s", 1.5, nil, true, []any{}, map[string]any{}},
		"list":      []string{"x", "y"},
		"maps":      []map[string]any{{"k": "v"}, nil, {}},
		"empty":     map[string]any{},
		"emptyList": []any{},
		"uni":       "héllo 😀",
		"int":       3, "i64": int64(-4), "u8": uint8(9), "f32": float32(0.5),
	}
}

// hostLazyFixture is a module in a realm made with opts, run on arguments
// converted by FromGo or by the eager reference.
type hostLazyFixture struct {
	*moduleFixture
}

func newHostLazyFixture(t *testing.T, src string, opts RealmOptions) hostLazyFixture {
	t.Helper()
	return hostLazyFixture{evalModuleWith(t, hostLazyDescribe+src, opts)}
}

func (f hostLazyFixture) fn(name string) Value {
	f.t.Helper()
	v, ok := f.env.GetBindingValue(name)
	require.True(f.t, ok, "export %q", name)
	return v
}

// callValues calls the exported function name with args and returns its
// result as a Go string (or the error's message).
func (f hostLazyFixture) callValues(name string, args ...Value) string {
	f.t.Helper()
	res, err := f.r.Call(f.fn(name), Undefined(), args)
	if err != nil {
		return "error: " + err.Error()
	}
	return f.r.ToGo(res).(string)
}

func (f hostLazyFixture) eager(v any) Value {
	f.t.Helper()
	out, err := eagerFromGo(f.r, v)
	require.NoError(f.t, err)
	return out
}

var hostLazyRealms = []struct {
	name string
	opts RealmOptions
}{
	{"own", RealmOptions{}},
	{"shared", RealmOptions{SharedIntrinsics: true}},
}

// TestHostLazyOperations applies every operation of hostLazyOps to a lazily
// and to an eagerly converted sample, twice (on the placeholder, then on the
// materialized and possibly modified result), and compares the operation's
// result and the complete state of the argument afterwards. The Go value is
// never written.
func TestHostLazyOperations(t *testing.T) {
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
	for _, realm := range hostLazyRealms {
		t.Run(realm.name, func(t *testing.T) {
			f := newHostLazyFixture(t, src, realm.opts)
			ops := f.fn("ops").AsObject()
			require.Equal(t, uint32(len(hostLazyOps)), ops.ArrayLength())
			for i, text := range hostLazyOps {
				op, err := ops.GetIndex(f.r, uint32(i))
				require.NoError(t, err)
				in := hostLazySample()
				lazy := mustFromGo(t, f.r, in)
				eager := f.eager(hostLazySample())
				for round := range 2 {
					want := f.callValues("run", op, eager)
					got := f.callValues("run", op, lazy)
					require.Equal(t, want, got, "op %s, round %d", text, round)
					// An op the engine cannot run compares equal and tests nothing.
					require.NotContains(t, want, "ReferenceError", "op %s", text)
					require.NotContains(t, want, "is not a function", "op %s", text)
				}
				require.Equal(t, hostLazySample(), in, "op %s wrote the Go value", text)
			}
		})
	}
}

// --- differential ----------------------------------------------------------------

// hostGenKeys are the keys random trees and the random walker draw from:
// canonical and non-canonical array indices, the largest index and the first
// non-index, keys that name prototype properties, non-ASCII and empty keys.
var hostGenKeys = []string{
	"a", "b", "c", "model", "0", "1", "2", "10", "01", "-1", "1.5",
	"4294967294", "4294967295", "é", "😀", "", "length", "__proto__", "constructor", "toString",
}

// hostTreeGen builds random JSON-shaped Go values from a seed.
type hostTreeGen struct {
	rnd    *rand.Rand
	shared []hostSharedTree // containers already built, reused to make DAGs
	budget int              // nodes the tree may still grow by, counted per path
}

// hostSharedTree is a container with the number of nodes it has per path
// (what eager conversion, JSON.stringify and describe expand it to).
type hostSharedTree struct {
	v any
	n int
}

// hostTreeBudget bounds a tree's size per path: reused containers would
// otherwise make it exponential.
const hostTreeBudget = 4000

func genHostTree(seed uint64) any {
	g := &hostTreeGen{rnd: rand.New(rand.NewPCG(seed, seed^0x5eed)), budget: hostTreeBudget}
	v, _ := g.container(0)
	return v
}

func (g *hostTreeGen) key() string {
	if g.rnd.IntN(4) == 0 {
		return "k" + strconv.Itoa(g.rnd.IntN(100))
	}
	return hostGenKeys[g.rnd.IntN(len(hostGenKeys))]
}

var hostGenStrings = []string{"", "x", "hello", "héllo", "😀 wide", "0", "12", "a\u0000b", "\"quoted\""}

func (g *hostTreeGen) str() string {
	if g.rnd.IntN(3) == 0 {
		return "s" + strconv.Itoa(g.rnd.IntN(1000))
	}
	return hostGenStrings[g.rnd.IntN(len(hostGenStrings))]
}

func (g *hostTreeGen) size() int {
	switch g.rnd.IntN(40) {
	case 0:
		return 70 // past maxShapeProps: a dictionary-mode map
	case 1, 2, 3:
		return 0
	}
	return 1 + g.rnd.IntN(5)
}

// value returns a random value and its node count per path.
func (g *hostTreeGen) value(depth int) (any, int) {
	g.budget--
	if depth < 6 && g.budget > 0 && g.rnd.IntN(2) == 0 {
		return g.container(depth)
	}
	switch g.rnd.IntN(7) {
	case 0, 1:
		return g.str(), 1
	case 2:
		return []float64{0, 1, -1, 2.5, 1e21, 4294967295, math.Copysign(0, -1)}[g.rnd.IntN(7)], 1
	case 3:
		return g.rnd.IntN(2) == 0, 1
	case 4:
		return nil, 1
	case 5:
		return g.rnd.IntN(100) - 50, 1
	}
	if len(g.shared) > 0 {
		if c := g.shared[g.rnd.IntN(len(g.shared))]; c.n <= g.budget {
			g.budget -= c.n
			return c.v, c.n
		}
	}
	return g.str(), 1
}

// container returns a random container and its node count per path.
func (g *hostTreeGen) container(depth int) (any, int) {
	var out any
	total := 1
	add := func() any {
		v, n := g.value(depth + 1)
		total += n
		return v
	}
	n := g.size()
	switch g.rnd.IntN(7) {
	case 0, 1:
		m := make(map[string]any, n)
		for range n {
			m[g.key()] = add()
		}
		out = m
	case 2, 3:
		s := make([]any, n)
		for i := range s {
			s[i] = add()
		}
		out = s
	case 4:
		m := make(map[string]string, n)
		for range n {
			m[g.key()] = g.str()
		}
		out = m
	case 5:
		s := make([]string, n)
		for i := range s {
			s[i] = g.str()
		}
		if n == 0 && g.rnd.IntN(2) == 0 {
			s = nil
		}
		out = s
	default:
		if g.rnd.IntN(2) == 0 {
			m := make(map[string][]string, n)
			for range n {
				list := make([]string, g.rnd.IntN(3))
				for i := range list {
					list[i] = g.str()
				}
				m[g.key()] = list
			}
			out = m
		} else {
			s := make([]map[string]any, n)
			for i := range s {
				if g.rnd.IntN(4) == 0 {
					continue // a nil map: null
				}
				m := make(map[string]any)
				for range g.rnd.IntN(4) {
					m[g.key()] = add()
				}
				s[i] = m
			}
			out = s
		}
	}
	g.shared = append(g.shared, hostSharedTree{out, total})
	return out, total
}

// hostLazyWalker is a seeded random walk over a value: reads along random
// paths (which materializes the nodes they reach, and only those), key
// listing, JSON, descriptors, writes, deletes, array mutation, integrity
// levels. It logs every result; two walks with the same seed over equal
// values log the same lines. It never stores a value into another, so the
// graph stays acyclic, and never enters the prototypes a __proto__ read
// returns: the eager and the lazy walk share the realm.
const hostLazyWalker = `
export function walk(x, seed, steps) {
	let s = seed >>> 0;
	const rnd = n => { s = (Math.imul(s, 1664525) + 1013904223) >>> 0; return (s >>> 8) % n; };
	const keys = KEYS;
	const show = v => v === null ? "null" : typeof v !== "object" ? typeof v + ":" + (v === 0 && 1 / v < 0 ? "-0" : String(v))
		: Array.isArray(v) ? "array:" + v.length : "object:" + Object.keys(v).length;
	const log = [];
	const stack = [x];
	let cur = x;
	for (let i = 0; i < steps; i++) {
		const op = rnd(22);
		let k = Array.isArray(cur) && rnd(2) === 0 ? String(rnd(cur.length + 1)) : keys[rnd(keys.length)];
		if (Array.isArray(cur) && k === "4294967294") k = "4294967295"; // no 2^32-1 long arrays to spread
		try {
			switch (op) {
			case 0: case 1: case 2: case 3: {
				const v = cur[k];
				log.push("get " + k + " " + show(v));
				if (v !== null && typeof v === "object" && v !== Object.prototype && v !== Array.prototype) {
					stack.push(v);
					if (rnd(3) !== 0) cur = v;
				}
				break;
			}
			case 4: log.push("keys " + Object.keys(cur).join()); break;
			case 5: log.push("has " + (k in cur) + Object.hasOwn(cur, k)); break;
			case 6: log.push("json " + JSON.stringify(cur)); break;
			case 7: cur[k] = rnd(5); log.push("set"); break;
			case 8: log.push("delete " + delete cur[k]); break;
			case 9: {
				const d = Object.getOwnPropertyDescriptor(cur, k);
				log.push("desc " + (d ? [d.writable, d.enumerable, d.configurable, show(d.value)].join() : "none"));
				break;
			}
			case 10: {
				const ks = [];
				for (const kk in cur) ks.push(kk);
				log.push("for-in " + ks.join());
				break;
			}
			case 11:
				if (Array.isArray(cur)) {
					switch (rnd(5)) {
					case 0: log.push("push " + cur.push(rnd(3))); break;
					case 1: log.push("pop " + show(cur.pop())); break;
					case 2: log.push("splice " + cur.splice(rnd(cur.length + 1), 1).map(show)); break;
					case 3: cur.length = rnd(cur.length + 2); log.push("length " + cur.length); break;
					case 4: log.push("concat " + cur.concat([1], cur).map(show)); break;
					}
				} else {
					log.push("assign " + Object.keys(Object.assign(cur, { [k]: "assigned" })).length);
				}
				break;
			case 12: cur = stack[rnd(stack.length)]; log.push("jump " + show(cur)); break;
			case 13: log.push("entries " + Object.entries(cur).map(([a, b]) => a + "=" + show(b)).join()); break;
			case 14: log.push("same " + (cur[k] === cur[k])); break;
			case 15: log.push("spread " + (Array.isArray(cur) ? [...cur].map(show) : Object.keys({ ...cur }))); break;
			case 16:
				if (rnd(6) === 0) Object.freeze(cur);
				log.push("frozen " + Object.isFrozen(cur) + Object.isExtensible(cur));
				break;
			case 17: cur[k] = rnd(2) === 0 ? { w: rnd(3) } : [rnd(3)]; log.push("set object"); break;
			case 18: log.push("string " + String(cur)); break;
			case 19:
				Object.defineProperty(cur, k, { value: "def", enumerable: rnd(2) === 0, configurable: true, writable: true });
				log.push("define");
				break;
			case 20: log.push("names " + Object.getOwnPropertyNames(cur).join()); break;
			case 21: log.push("proto " + (Object.getPrototypeOf(cur) === Array.prototype) + (cur instanceof Object)); break;
			}
		} catch (e) {
			log.push("throws " + e.name + ": " + e.message);
		}
		if (cur === null || typeof cur !== "object") cur = stack[rnd(stack.length)];
	}
	log.push("final " + JSON.stringify(x));
	log.push("describe " + describe(x));
	return log.join("\n");
}
`

// TestHostLazyDifferential converts random Go trees lazily and eagerly, runs
// the same seeded random walk over both and compares the walks' logs, the
// values' final states (walked through [[Get]]) and their exports; the Go
// trees are never written. One realm serves all trees, so periods, chunk
// reuse and the shape and string caches are exercised across conversions.
func TestHostLazyDifferential(t *testing.T) {
	keys, err := json.Marshal(hostGenKeys)
	require.NoError(t, err)
	src := strings.Replace(hostLazyWalker, "KEYS", string(keys), 1)
	seeds := 1000
	if testing.Short() {
		seeds = 100
	}
	for _, realm := range hostLazyRealms {
		t.Run(realm.name, func(t *testing.T) {
			f := newHostLazyFixture(t, src, realm.opts)
			for seed := range uint64(seeds) {
				in := genHostTree(seed)
				lazy := mustFromGo(t, f.r, in)
				eager := f.eager(genHostTree(seed))
				steps := IntValue(10 + int(seed%50))
				want := f.callValues("walk", eager, IntValue(int(seed)), steps)
				got := f.callValues("walk", lazy, IntValue(int(seed)), steps)
				require.Equal(t, want, got, "seed %d", seed)
				gotExp := f.r.ToGo(lazy)
				if src, ok := lazy.AsObject().HostValue(); ok {
					assertSameGo(t, src, gotExp)
				}
				require.Equal(t, f.r.ToGo(eager), hostExportForm(gotExp), "seed %d: export", seed)
				require.Equal(t, walkToGo(f.r, eager), walkToGo(f.r, lazy), "seed %d: state", seed)
				require.Equal(t, genHostTree(seed), in, "seed %d: the Go value was written", seed)
			}
		})
	}
}

// hostExportForm maps an export in which unmodified nodes are their Go values
// onto the types ToGo gives a converted value: []any and map[string]any
// containers, int64 or float64 numbers. A nil slice becomes an empty one,
// as FromGo converts it to an empty array.
func hostExportForm(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = hostExportForm(e)
		}
		return out
	case map[string]string:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = e
		}
		return out
	case map[string][]string:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = hostExportForm(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = hostExportForm(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = hostExportForm(e)
		}
		return out
	case int:
		return int64(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<63 && (x != 0 || !math.Signbit(x)) {
			return int64(x)
		}
		return x
	}
	return v
}

// --- export identity ----------------------------------------------------------------

// TestHostLazyExportIdentity: a node JavaScript did not modify exports as the
// Go value it came from, touched or not; a modified node exports a snapshot
// in which its unmodified children still are their Go values. An array that
// gets a named property loses its Go value (the property replaces the slots
// that held it) and exports an equal copy.
func TestHostLazyExportIdentity(t *testing.T) {
	f := newHostLazyFixture(t, `
export function touch(x) { return Object.keys(x).length + x.list.length + x.messages[0].role + x.maps[0].k + x.headers["X-A"]; }
export function rewrite(x) { x.model = x.model; x.list[0] = x.list[0]; x.list.push("z"); x.list.pop(); return ""; }
export function write(x) { x.model = "changed"; return ""; }
export function named(x) { x.list.tag = 1; x.messages.tag = 1; return ""; }
export function dict(x) { Object.defineProperty(x.maps, "k", { get() { return 1; } }); delete x.maps.k; return ""; }
`, RealmOptions{})
	r := f.r
	child := func(v Value, k string) Value {
		out, err := v.AsObject().GetProp(r, key(r, k))
		require.NoError(t, err)
		return out
	}

	in := hostLazySample()
	x := mustFromGo(t, r, in)
	assertSameGo(t, in, r.ToGo(x))
	f.callValues("touch", x)
	assertSameGo(t, in, r.ToGo(x))
	assertSameGo(t, in["list"], r.ToGo(child(x, "list")))
	assertSameGo(t, in["maps"], r.ToGo(child(x, "maps")))
	assertSameGo(t, in["headers"], r.ToGo(child(x, "headers")))
	// Writing what a property already holds is no modification.
	f.callValues("rewrite", x)
	assertSameGo(t, in, r.ToGo(x))

	f.callValues("write", x)
	out := r.ToGo(x).(map[string]any)
	assert.Equal(t, "changed", out["model"])
	for _, k := range []string{"messages", "list", "maps", "headers", "query", "empty"} {
		if k == "empty" {
			assert.Equal(t, map[string]any{}, out[k]) // an empty map converts to an ordinary object
			continue
		}
		assertSameGo(t, in[k], out[k])
	}
	assert.Equal(t, int64(3), out["int"])

	// Each slice type keeps its Go value in the node's empty slots.
	for _, k := range []string{"messages", "list", "maps"} {
		o := child(x, k).AsObject()
		_, ok := boxedSliceIn(o.slots)
		assert.True(t, ok, "%s holds its slice", k)
	}
	f.callValues("named", x)
	for _, k := range []string{"messages", "list"} {
		v := child(x, k)
		_, ok := v.AsObject().HostValue()
		assert.False(t, ok, "%s has a named property", k)
		assert.Equal(t, walkToGo(r, f.eager(in[k])), r.ToGo(v))
	}
	assert.Equal(t, []any{"x", "y"}, r.ToGo(child(x, "list")))
	f.callValues("dict", x)
	_, ok := child(x, "maps").AsObject().HostValue()
	assert.False(t, ok, "a dictionary-mode array is modified")
	assert.Equal(t, []any{map[string]any{"k": "v"}, nil, map[string]any{}}, walkToGo(r, child(x, "maps")))
	assertSameGo(t, in["maps"].([]map[string]any)[0], r.ToGo(child(x, "maps")).([]any)[0])

	// Roots of every container type.
	for _, v := range []any{
		map[string]string{"a": "b"}, map[string][]string{"a": {"b"}}, []string{"a"},
		[]any{1, "x"}, []map[string]any{{"a": 1}},
	} {
		assertSameGo(t, v, r.ToGo(mustFromGo(t, r, v)))
	}
}

// TestHostLazyEntryErrors: a value FromGo cannot convert (here a struct with
// no host hook installed) no longer fails the whole conversion; it becomes a
// throwing accessor where it sits, found by key listing like any property.
func TestHostLazyEntryErrors(t *testing.T) {
	f := newHostLazyFixture(t, `
export function probe(x) {
	const out = [];
	const tryIt = (name, fn) => { try { out.push(name + " " + describe(fn())); } catch (e) { out.push(name + " throws " + e.name + ": " + e.message); } };
	tryIt("ok", () => x.ok);
	tryIt("bad", () => x.bad);
	tryIt("keys", () => Object.keys(x));
	tryIt("desc", () => typeof Object.getOwnPropertyDescriptor(x, "bad").get);
	tryIt("json", () => JSON.stringify(x));
	tryIt("elem", () => x.list[1]);
	tryIt("elemOk", () => x.list[0]);
	tryIt("spread", () => ({ ...x }));
	return out.join("\n");
}
`, RealmOptions{})
	type opaque chan int // a type FromGo does not convert, nor encoding/json
	in := map[string]any{"ok": 1, "bad": make(opaque), "list": []any{"fine", make(opaque)}}
	x := mustFromGo(t, f.r, in)
	msg := "TypeError: engine: FromGo: unsupported Go type engine.opaque"
	assert.Equal(t, strings.Join([]string{
		"ok 1",
		"bad throws " + msg,
		`keys A[3]{0:ecw="bad",1:ecw="list",2:ecw="ok",length:w=3,}`,
		`desc "function"`,
		"json throws " + msg,
		"elem throws " + msg,
		`elemOk "fine"`,
		"spread throws " + msg,
	}, "\n"), f.callValues("probe", x))
	_, err := f.r.ToGoStrict(x)
	assert.ErrorContains(t, err, "unsupported Go type")
	assert.Equal(t, map[string]any{"ok": int64(1), "bad": nil, "list": []any{"fine", nil}}, f.r.ToGo(x))
}

// TestHostLazyCycles: a Go map that contains itself converts to an unbounded
// tree, one JavaScript object per path, materialized as far as it is walked
// (eager conversion failed with ErrFromGoDepth). Export of an unmodified node
// is the Go map itself, cycle included, while the walked part is at most
// maxHostVerifyDepth deep; past that the export is a snapshot down to where
// the verification reaches again (the same value, without the identity).
// JSON.stringify walks until the stringifier's depth limit, a catchable
// error.
func TestHostLazyCycles(t *testing.T) {
	f := newHostLazyFixture(t, `
export function walk(x, n) {
	let cur = x;
	for (let i = 0; i < n; i++) cur = cur.self;
	return [cur.a, cur !== x, x.self !== x, x.self === x.self, x.list[0].self.a].join();
}
export function json(x) { try { return JSON.stringify(x); } catch (e) { return e.name; } }
`, RealmOptions{})
	cyclic := func() map[string]any {
		m := map[string]any{"a": "leaf"}
		m["self"] = m
		m["list"] = []any{m}
		return m
	}
	m := cyclic()
	x := mustFromGo(t, f.r, m)
	assert.Equal(t, "leaf,true,true,true,leaf", f.callValues("walk", x, IntValue(maxHostVerifyDepth-2)))
	assertSameGo(t, m, f.r.ToGo(x))
	assert.Equal(t, "RangeError", f.callValues("json", x))
	// JSON.stringify went jsonMaxDepth levels down x.list before it threw;
	// x.self is as deep as the walk left it.
	out := f.r.ToGo(x).(map[string]any)
	assert.Equal(t, "leaf", out["a"])
	assertSameGo(t, m, out["self"])

	m = cyclic()
	x = mustFromGo(t, f.r, m)
	assert.Equal(t, "leaf,true,true,true,leaf", f.callValues("walk", x, IntValue(1000)))
	exp, levels := f.r.ToGo(x), 0
	for ; levels <= 1000; levels++ {
		mm, ok := exp.(map[string]any)
		require.True(t, ok, "level %d", levels)
		if sameEface(mm, m) {
			break
		}
		assert.Equal(t, "leaf", mm["a"])
		exp = mm["self"]
	}
	assert.Greater(t, levels, 1000-maxHostVerifyDepth)
	assert.LessOrEqual(t, levels, 1000)

	// The eager reference, on a cycle with one path.
	n := map[string]any{}
	n["n"] = n
	_, err := eagerFromGo(f.r, n)
	assert.ErrorIs(t, err, ErrFromGoDepth)
}

// TestHostLazyFrozen: freezing a placeholder materializes it and freezes that
// one level (Object.freeze is shallow); its children stay placeholders and
// freezing changes no export.
func TestHostLazyFrozen(t *testing.T) {
	f := newHostLazyFixture(t, `
export function freeze(x) {
	Object.freeze(x);
	const errs = [];
	for (const fn of [() => { x.model = 1; }, () => { x.fresh = 1; }, () => { delete x.n; }, () => { x.headers["X-C"] = "3"; }]) {
		try { fn(); errs.push("ok"); } catch (e) { errs.push(e.name); }
	}
	return [Object.isFrozen(x), Object.isFrozen(x.headers), ...errs].join();
}
`, RealmOptions{})
	in := hostLazySample()
	x := mustFromGo(t, f.r, in)
	assert.Equal(t, "true,false,TypeError,TypeError,TypeError,ok", f.callValues("freeze", x))
	list, err := x.AsObject().GetProp(f.r, key(f.r, "list"))
	require.NoError(t, err)
	assert.Equal(t, hostSentinelKey, list.AsObject().shape.key, "children stay placeholders")
	out := f.r.ToGo(x).(map[string]any)
	assert.Equal(t, map[string]any{"X-A": "1", "X-B": "2", "X-C": "3"}, out["headers"])
	assertSameGo(t, in["list"], out["list"])
}

// --- lifetime, goroutines, interrupts ----------------------------------------------

// TestHostLazyRetainedAcrossCalls: values JavaScript keeps from earlier calls
// (materialized nodes, untouched placeholders, array nodes whose Go slice
// only their slots still reference, containers a Go function returned during
// a call) keep working after later calls have started new periods and the
// collector has run; nothing but the kept nodes references the Go values.
func TestHostLazyRetainedAcrossCalls(t *testing.T) {
	src := `
const kept = [];
export function keep(x, mk) {
	kept.push(x, x.list, x.maps, x.messages[0], x.query, mk());
	return "";
}
export function check() { return describe(kept); }
`
	run := func(conv func(*Realm, any) (Value, error)) string {
		f := newHostLazyFixture(t, src, RealmOptions{})
		for i := range 40 {
			x, err := conv(f.r, hostLazySampleN(i))
			require.NoError(t, err)
			mk := ObjectValue(f.r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
				return conv(r, map[string]any{"made": []any{strconv.Itoa(i), map[string]any{"n": float64(i)}}})
			}))
			require.Equal(t, "", f.callValues("keep", x, mk))
			if i%8 == 0 {
				hostLazyChurn()
			}
		}
		hostLazyChurn()
		return f.callValues("check")
	}
	want := run(eagerFromGo)
	got := run((*Realm).FromGo)
	assert.Equal(t, want, got)
}

// hostLazySampleN is hostLazySample with contents that depend on i.
func hostLazySampleN(i int) map[string]any {
	m := hostLazySample()
	s := strconv.Itoa(i)
	m["model"] = "model-" + s
	m["list"] = []string{"x" + s, "y" + s, strings.Repeat("z", i)}
	m["maps"] = []map[string]any{{"k": s}, nil, {"i": float64(i)}}
	m["query"] = map[string][]string{"q": {s}}
	return m
}

// hostLazyChurn collects twice around a burst of allocations of the sizes of
// boxed slice headers and nodes, so memory freed by mistake would be reused.
func hostLazyChurn() {
	runtime.GC()
	sink := make([]any, 0, 4096)
	for i := range 4096 {
		sink = append(sink, []any{i}, make([]Value, 6))
	}
	runtime.KeepAlive(sink)
	runtime.GC()
}

// TestHostLazyGoroutines is run under -race by the gates. Realms on several
// goroutines convert one shared Go value and read it concurrently
// (materialization only reads Go values, and each realm reads them on its own
// goroutine), and each realm's own argument is written by the test goroutine
// between calls, once JavaScript no longer holds it; values kept across calls
// keep working. A read of a Go value off its realm's goroutine or outside a
// call, or any write to one, is a data race the detector reports.
func TestHostLazyGoroutines(t *testing.T) {
	src := `
let kept;
export function read(shared, own) {
	if (kept === undefined) kept = shared;
	return [Object.keys(shared).length, shared.messages[0].role, own.list.join(), JSON.stringify(own.maps), kept.headers["X-A"], kept.list[1]].join();
}
`
	shared := hostLazySample()
	const workers, calls = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		f := newHostLazyFixture(t, src, hostLazyRealms[w%2].opts)
		read := f.fn("read")
		own := hostLazySampleN(w)
		turn, done := make(chan struct{}), make(chan struct{})
		wg.Add(2)
		go func() { // the realm's goroutine
			defer wg.Done()
			defer close(done)
			for i := range calls {
				<-turn
				sv, err := f.r.FromGo(shared)
				if err != nil {
					errs <- err
					return
				}
				ov, err := f.r.FromGo(own)
				if err != nil {
					errs <- err
					return
				}
				res, err := f.r.Call(read, Undefined(), []Value{sv, ov})
				if err != nil {
					errs <- err
					return
				}
				want := fmt.Sprintf(`%d,user,%s,%s,1,y`, len(shared), strings.Join(own["list"].([]string), ","), mustJSON(own["maps"]))
				if got := f.r.ToGo(res); got != want {
					errs <- fmt.Errorf("worker %d call %d: got %v, want %s", w, i, got, want)
					return
				}
				done <- struct{}{}
			}
		}()
		go func() { // the host: writes its argument between calls
			defer wg.Done()
			for i := range calls {
				turn <- struct{}{}
				if _, ok := <-done; !ok {
					return
				}
				own["list"] = []string{strconv.Itoa(i)}
				own["maps"].([]map[string]any)[0]["k"] = strconv.Itoa(i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestHostLazyInterrupt: materialization converts one level per touch, so a
// hook that walks a large argument runs in the interpreter, which observes an
// interrupt; afterwards the realm converts and reads arguments again.
func TestHostLazyInterrupt(t *testing.T) {
	f := newHostLazyFixture(t, `
export function spin(x) {
	let n = 0;
	for (let i = 0; ; i = (i + 1) % x.length) n += x[i].k.length;
}
export function sum(x) { let n = 0; for (const m of x) n += m.k.length; return String(n); }
`, RealmOptions{})
	big := make([]any, 200000)
	for i := range big {
		big[i] = map[string]any{"k": "v" + strconv.Itoa(i%10)}
	}
	timer := time.AfterFunc(20*time.Millisecond, func() { f.r.Interrupt("stop") })
	defer timer.Stop()
	_, err := f.r.Call(f.fn("spin"), Undefined(), []Value{mustFromGo(t, f.r, big)})
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	f.r.ClearInterrupt()
	assert.Equal(t, "400000", f.callValues("sum", mustFromGo(t, f.r, big)))
}

// --- engine invariants ---------------------------------------------------------------

// TestHostLazyPlaceholderInvariants: a placeholder misses every inline cache
// (its shape is the host sentinel, in dictionary mode with no dictionary);
// its first touch lays it out under the shape eager conversion builds, so a
// site that read a placeholder then hits for materialized arguments and plain
// objects of that shape alike.
func TestHostLazyPlaceholderInvariants(t *testing.T) {
	f := newHostLazyFixture(t, `
export function get(x) { return String(x.model) + x.n; }
export function plain() {
	return { "01": 0, "4294967295": 0, a: "first", b: "second", empty: 0, emptyList: 0, f32: 0, headers: 0, i64: 0, int: 0,
		list: 0, maps: 0, messages: 0, model: "plain", n: 1, nil: 0, query: 0, stream: 0, u8: 0, uni: 0, upstreamModel: 0 };
}
`, RealmOptions{})
	r := f.r
	x := mustFromGo(t, r, hostLazySample())
	o := x.AsObject()
	h := r.fromGo.heap
	require.NotNil(t, h)
	assert.Same(t, &h.sentinel, o.shape)
	assert.True(t, o.shape.isDict)
	assert.Equal(t, flagExtensible|flagDict|flagHasLazy|flagHostNode, o.flags)
	assert.Nil(t, o.dictTable())
	assert.Empty(t, o.slots)
	assert.Empty(t, o.elems())
	assert.Nil(t, o.Internal(), "a map node's Go map is not its payload")

	assert.Equal(t, "gpt-42", f.callValues("get", x))
	eager := f.eager(hostLazySample())
	assert.Same(t, eager.AsObject().shape, o.shape, "materialized under the eager shape")
	assert.Zero(t, o.flags&(flagDict|flagHasLazy))
	assert.NotZero(t, o.flags&flagHostNode)
	assert.Nil(t, o.Internal())
	for range 3 {
		assert.Equal(t, "gpt-42", f.callValues("get", mustFromGo(t, r, hostLazySample())))
		assert.Equal(t, "gpt-42", f.callValues("get", eager))
		p, err := r.Call(f.fn("plain"), Undefined(), nil)
		require.NoError(t, err)
		assert.Same(t, o.shape, p.AsObject().shape, "a literal with the same keys in sorted order")
		assert.Equal(t, "plain1", f.callValues("get", p))
	}

	// Array nodes: length needs no materialization, and the Go slice is
	// not the payload.
	list, err := o.GetProp(r, key(r, "list"))
	require.NoError(t, err)
	lo := list.AsObject()
	assert.Same(t, &h.arraySentinel.shape, lo.shape)
	assert.Equal(t, uint32(2), lo.ArrayLength())
	assert.Nil(t, lo.Internal())
	assert.Same(t, &h.arraySentinel.shape, lo.shape, "length does not materialize")
	assert.True(t, lo.IsDenseArray())
	assert.NotSame(t, &h.arraySentinel.shape, lo.shape)
}

// TestHostLazySharedStrings: the same Go string converted twice in a period
// becomes one String (sharing an immutable string is unobservable); equal
// contents in different Go strings do not share, for ASCII and non-ASCII alike.
// The keys sort so that no other string is converted between model and
// upstream: one whose cache slot collided would evict model.
func TestHostLazySharedStrings(t *testing.T) {
	r := NewRealm()
	model := strings.Repeat("m", 3)
	other := strings.Repeat("m", 3)
	uni := "é" + model
	x := mustFromGo(t, r, map[string]any{"model": model, "upstream": model, "zother": other, "v1": uni, "v2": uni})
	get := func(k string) *String {
		v, err := x.AsObject().GetProp(r, key(r, k))
		require.NoError(t, err)
		return v.AsString()
	}
	assert.Same(t, get("model"), get("upstream"))
	assert.NotSame(t, get("model"), get("zother"))
	assert.True(t, get("model").Equals(get("zother")))
	assert.Same(t, get("v1"), get("v2"))
	assert.True(t, get("v1").Equals(get("v2")))
}

// TestHostLazyShapePrediction: a map is laid out by the key set the heap
// predicts from the maps of its size before it (hostHeap.shapes). Key sets
// that alternate, differ in one key, hold an array index, outnumber the
// prediction's ways or share its slot with another size, in map[string]any
// and map[string]string values, lay out as the eager conversion does, cold
// and with the prediction warm, and maps with one named key set share one
// shape.
func TestHostLazyShapePrediction(t *testing.T) {
	sets := [][]string{
		{"a", "b", "c"}, {"x", "y", "z"}, {"a", "b", "c"}, {"a", "b", "d"},
		{"a", "b", "0"}, {"c", "b", "a"}, {"p", "q", "r"}, {"s", "t", "u"},
		{"a", "b", "c"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
		{"x", "y", "z"}, {"b", "a"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
	}
	gen := func() any {
		list := make([]any, 0, 2*len(sets))
		for i, set := range sets {
			m := make(map[string]any, len(set))
			ms := make(map[string]string, len(set))
			for j, k := range set {
				m[k] = i*100 + j
				ms[k] = k + strconv.Itoa(i)
			}
			list = append(list, m, ms)
		}
		return list
	}
	for _, realm := range hostLazyRealms {
		t.Run(realm.name, func(t *testing.T) {
			f := newHostLazyFixture(t, `export function show(x) { return describe(x); }`, realm.opts)
			for range 2 {
				lazy := mustFromGo(t, f.r, gen())
				require.Equal(t, f.callValues("show", f.eager(gen())), f.callValues("show", lazy))
				shapes := map[string]*Shape{}
				for i := range 2 * len(sets) {
					el, err := lazy.AsObject().GetProp(f.r, key(f.r, strconv.Itoa(i)))
					require.NoError(t, err)
					var named []string
					for _, k := range sets[i/2] {
						if _, ok := parseArrayIndex(k); !ok {
							named = append(named, k)
						}
					}
					slices.Sort(named)
					id := strings.Join(named, ",")
					if s, ok := shapes[id]; ok {
						assert.Same(t, s, el.AsObject().shape, "element %d %v", i, sets[i/2])
					} else {
						shapes[id] = el.AsObject().shape
					}
				}
			}
		})
	}
}

// TestHostLazyArrayFastPaths: the dense-array fast paths of the Array
// builtins (plainArray) apply to placeholders, which they materialize first.
func TestHostLazyArrayFastPaths(t *testing.T) {
	r := NewRealm()
	list := mustFromGo(t, r, []any{"a", map[string]any{"k": 1.0}}).AsObject()
	assert.True(t, plainArray(list))
	assert.Zero(t, list.flags&flagHasLazy, "materialized")
	assert.Len(t, list.elems(), 2)
	assert.False(t, plainArray(mustFromGo(t, r, map[string]any{"a": 1}).AsObject()))
}

// TestHostLazyIndexFreeProto: plainArray materializes a host placeholder on
// a dense array's prototype chain and then judges it as any object. Behind
// a named-only map the array keeps the fast paths from the first call on
// (Array.prototype.reverse.call reaches the map through no lookup, so
// nothing else would ever materialize it); behind a map with an index key
// or a slice it loses them.
func TestHostLazyIndexFreeProto(t *testing.T) {
	r := NewRealm()
	behind := func(v any, between bool) *Object {
		p := mustFromGo(t, r, v).AsObject()
		require.Equal(t, hostSentinelKey, p.shape.key)
		if between {
			p = r.NewObjectWithProto(p)
		}
		a := r.NewArray(IntValue(1), IntValue(2))
		require.True(t, a.SetPrototypeOf(r, p))
		return a
	}
	for _, between := range []bool{false, true} {
		named := behind(map[string]any{"a": "A"}, between)
		assert.True(t, plainArray(named), "named-only map, between=%v", between)
		assert.True(t, plainArray(named), "named-only map, again")
		assert.False(t, plainArray(behind(map[string]any{"a": "A", "1": "one"}, between)), "map with an index key, between=%v", between)
		assert.False(t, plainArray(behind([]any{"x"}, between)), "slice, between=%v", between)
	}
	f := evalModule(t, `export const rev = p => { const a = [1, , 3]; Object.setPrototypeOf(a, p); const A = Array.prototype; return A.join.call(A.reverse.call(a)); };`)
	rev, ok := f.env.GetBindingValue("rev")
	require.True(t, ok)
	for _, c := range []struct {
		proto any
		want  string
	}{
		{map[string]any{"a": "A"}, "3,,1"},
		{map[string]any{"1": "one"}, "3,one,1"},
	} {
		out, err := f.r.Call(rev, Undefined(), []Value{mustFromGo(t, f.r, c.proto)})
		require.NoError(t, err)
		assert.Equal(t, c.want, out.String())
	}
}

// TestHostLazyProtocolFastPaths: the shape-probe answers of the protocol
// guards (protocol.go, iter.go) hold for placeholders as for the eager
// values: for-of and spread iterate a host slice by index, species and
// @@isConcatSpreadable resolve to the defaults and ToPrimitive finds no
// @@toPrimitive, the last three without materializing anything.
func TestHostLazyProtocolFastPaths(t *testing.T) {
	for _, rk := range hostLazyRealms {
		t.Run(rk.name, func(t *testing.T) {
			r := NewRealmWith(rk.opts)
			h := func(v any) *Object {
				o := mustFromGo(t, r, v).AsObject()
				require.Equal(t, hostSentinelKey, o.shape.key)
				return o
			}
			list := h([]any{"a", 1.0})
			assert.True(t, r.fastIterable(ObjectValue(list)))
			assert.NotEqual(t, hostSentinelKey, list.shape.key, "iterating reads the elements")
			assert.Len(t, list.elems(), 2)

			list = h([]any{"a", 1.0})
			ctor, err := r.arraySpeciesCtor(list)
			require.NoError(t, err)
			assert.Nil(t, ctor)
			assert.True(t, r.lacksSpreadable(list, r.spreadableProtosClean()))
			m := h(map[string]any{"a": 1.0})
			assert.True(t, r.lacksWellKnown(m, SymbolKey(SymToPrimitive)))
			assert.True(t, r.lacksWellKnown(m, SymbolKey(SymToStringTag)))
			assert.False(t, r.lacksWellKnown(m, key(r, "a")), "string keys are unknown until materialized")
			assert.Equal(t, hostSentinelKey, list.shape.key)
			assert.Equal(t, hostSentinelKey, m.shape.key)
		})
	}
}

// TestHostLazySteadyState: a hook runtime that converts a same-shaped
// argument and reads all of it on every call allocates no more than eager
// conversion did (each call's nodes come from one chunk per kind); an
// argument the hook does not read costs its root only.
func TestHostLazySteadyState(t *testing.T) {
	f := newHostLazyFixture(t, `
export function all(x) { return describe(x) ? "" : "x"; }
export function none(x) { return ""; }
`, RealmOptions{})
	arg := hostLazySample()
	call := func(name string, conv func(*Realm, any) (Value, error)) float64 {
		run := func() {
			x, err := conv(f.r, arg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Call(f.fn(name), Undefined(), []Value{x}); err != nil {
				t.Fatal(err)
			}
		}
		for range 4 {
			run()
		}
		return testing.AllocsPerRun(50, run)
	}
	scalar := func(*Realm, any) (Value, error) { return IntValue(1), nil }
	eagerAll, all := call("all", eagerFromGo), call("all", (*Realm).FromGo)
	eagerNone, none := call("none", eagerFromGo), call("none", (*Realm).FromGo)
	calls := call("none", scalar)
	t.Logf("allocs per call: all %.0f (eager %.0f), none %.0f (eager %.0f), a call %.0f", all, eagerAll, none, eagerNone, calls)
	assert.LessOrEqual(t, all, eagerAll)
	assert.LessOrEqual(t, none-calls, 1.0, "the root")
}

// TestHostStrDedupeIgnoresAddresses checks that the String headers the host
// heap shares depend on which Go strings are the same, not on where they
// are: the same sequence of Go strings, copied to fresh memory each time,
// allocates as many headers, and only a repeat of the same Go string shares
// one.
func TestHostStrDedupeIgnoresAddresses(t *testing.T) {
	words := []string{"vendor-a", "create", "openai_responses", "json", "1536x1024",
		"system", "user", "input_text", "input_image", "abc123", "gzip", "vendor"}
	seq := []int{0, 1, 2, 0, 3, 0, 4, 5, 6, 7, 8, 7, 9, 10, 11, 6}
	used := -1
	for range 20 {
		h := NewRealm().hostHeap()
		g := make([]string, len(words))
		for i, w := range words {
			g[i] = strings.Clone(w)
		}
		for _, i := range seq {
			h.str(g[i])
		}
		if used >= 0 {
			require.Equal(t, used, h.used.strings, "String headers")
		}
		used = h.used.strings
		require.Same(t, h.str(g[0]), h.str(g[0]))
		require.NotSame(t, h.str(g[0]), h.str(strings.Clone(g[0])))
	}
}

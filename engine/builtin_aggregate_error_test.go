package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAggregateError(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"basic", `const e = new AggregateError([1, "a"], "msg"); return [e.errors.join(), e.message, e.name, e instanceof AggregateError, e instanceof Error, Array.isArray(e.errors), String(e)].join()`, "1,a,msg,AggregateError,true,true,true,AggregateError: msg"},
		{"call", `const e = AggregateError(new Set([1, 2])); return [e.errors.length, e.message, Object.hasOwn(e, "message"), e.constructor === AggregateError].join()`, "2,,false,true"},
		{"cause", `const e = new AggregateError([], "m", {cause: 0}); return [Object.hasOwn(e, "cause"), e.cause, Object.hasOwn(new AggregateError([], "m", {}), "cause")].join()`, "true,0,false"},
		{"errors attrs", `const d = Object.getOwnPropertyDescriptor(new AggregateError([]), "errors"); return [d.enumerable, d.writable, d.configurable].join()`, "false,true,true"},
		{"fresh array", `const a = [1]; return new AggregateError(a).errors !== a`, "true"},
		{"order", `const log = []; const msg = {toString() { log.push("msg"); return "m"; }}; const it = {[Symbol.iterator]() { log.push("iter"); return [][Symbol.iterator](); }}; new AggregateError(it, msg, {get cause() { log.push("cause"); }}); return log.join()`, "msg,cause,iter"},
		{"not iterable", `return new AggregateError(1)`, "!TypeError"},
		{"no errors", `return new AggregateError()`, "!TypeError"},
		{"shape", `return [AggregateError.length, AggregateError.name, Object.getPrototypeOf(AggregateError) === Error, Object.getPrototypeOf(AggregateError.prototype) === Error.prototype, AggregateError.prototype.name, AggregateError.prototype.message === "", Object.hasOwn(AggregateError.prototype, "errors"), Object.prototype.toString.call(new AggregateError([]))].join()`, "2,AggregateError,true,true,AggregateError,true,false,[object Error]"},
		{"throw and catch", `try { throw new AggregateError([new TypeError("a")], "all failed"); } catch (e) { return e.errors[0].message + "/" + e.message; }`, "a/all failed"},
	})
}

// TestAggregateErrorNewTarget covers GetPrototypeFromConstructor(newTarget).
func TestAggregateErrorNewTarget(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	nt := r.NewNativeFunction(AtomEmpty, 0, nil)
	require.NoError(t, nt.SetProp(r, StringKey(AtomPrototype), ObjectValue(proto)))
	v, err := aggregateErrorConstruct(r, []Value{ObjectValue(r.NewArray())}, nt)
	require.NoError(t, err)
	assert.Equal(t, proto, v.AsObject().Proto())
	assert.Equal(t, ClassError, v.AsObject().class)
}

func TestObjectGroupBy(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"basic", `const g = Object.groupBy([1, 2, 3, 4, 5], (v, i) => v % 2 ? "odd" : i); return Object.keys(g).map(k => k + ":" + g[k].join("")).join()`, "1:2,3:4,odd:135"},
		{"insertion order", `const g = Object.groupBy(["b", "a", "b", "c"], v => v); return Object.keys(g).join()`, "b,a,c"},
		{"null prototype", `const g = Object.groupBy([1], () => "toString"); return [Object.getPrototypeOf(g), g.toString.join()].join()`, ",1"},
		{"key coercion", `const k = {toString() { return "k"; }}; const s = Symbol("s"); const g = Object.groupBy([1, 2, 3], v => v === 1 ? k : v === 2 ? s : 1); return [g.k.join(), g[s].join(), g["1"].join(), Object.keys(g).length].join()`, "1,2,3,2"},
		{"-0 key", `return Object.keys(Object.groupBy([1], () => -0)).join()`, "0"},
		{"string", `const g = Object.groupBy("aab", c => c); return g.a.length + "," + g.b.length`, "2,1"},
		{"empty", `return Object.keys(Object.groupBy([], () => 1)).length`, "0"},
		{"callback throws closes", `const log = []; const it = {[Symbol.iterator]() { return {next() { return {value: 1, done: false}; }, return() { log.push("r"); return {}; }}; }}; try { Object.groupBy(it, () => { throw new Error("cb"); }); } catch (e) { log.push(e.message); } return log.join()`, "r,cb"},
		{"key coercion throws closes", `const log = []; const it = {[Symbol.iterator]() { return {next() { return {value: 1, done: false}; }, return() { log.push("r"); return {}; }}; }}; try { Object.groupBy(it, () => ({toString() { throw new Error("key"); }})); } catch (e) { log.push(e.message); } return log.join()`, "r,key"},
		{"not callable", `return Object.groupBy([], 1)`, "!TypeError"},
		{"nullish", `return Object.groupBy(undefined, () => 1)`, "!TypeError"},
		{"length", `return Object.groupBy.length + Object.groupBy.name`, "2groupBy"},
	})
}

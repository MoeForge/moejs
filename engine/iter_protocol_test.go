package engine

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIteratorRecordNonCallableNext runs verified bytecode whose iterator
// record holds an array as its next method, as FuzzBytecode found. The
// generic step must reject it before reading it as a function.
func TestIteratorRecordNonCallableNext(t *testing.T) {
	for _, op := range []bytecode.Op{bytecode.IterNext, bytecode.IterValue, bytecode.IterRest} {
		t.Run(op.String(), func(t *testing.T) {
			fn := &bytecode.Function{Name: "f", NumRegs: 3, Strict: true, Code: []uint32{
				bytecode.EncodeABx(bytecode.NewObject, 0, 0),
				bytecode.EncodeABx(bytecode.NewArray, 1, 0),
				bytecode.EncodeABC(op, 0, 0, 0),
				bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0),
			}}
			require.NoError(t, bytecode.VerifyRegisters(fn))
			r := NewRealm()
			f, _ := testClosure(r, fn)
			_, err := r.CallObject(f, Undefined(), nil)
			assertErrorKind(t, err, KindTypeError, "iterator.next is not a function")
		})
	}
}

// TestIterNextNumberPosNonArray runs verified bytecode whose iterator
// record pairs an object other than an array with a number position: the
// global object after two rebuilds (removeNamed) and an ordinary object.
// IterNext's array fast mode must leave both to the generic step, which
// finds them exhausted (9; a value would give 7).
func TestIterNextNumberPosNonArray(t *testing.T) {
	fn := &bytecode.Function{Name: "f", NumRegs: 3, Strict: true, Code: []uint32{
		bytecode.EncodeABC(bytecode.LoadThis, 0, 0, 0),
		bytecode.EncodeAsBx(bytecode.LoadInt, 1, 0),
		bytecode.EncodeAsBx(bytecode.IterNext, 0, 2),
		bytecode.EncodeAsBx(bytecode.LoadInt, 2, 7),
		bytecode.EncodeABC(bytecode.Ret, 2, 0, 0),
		bytecode.EncodeAsBx(bytecode.LoadInt, 2, 9),
		bytecode.EncodeABC(bytecode.Ret, 2, 0, 0),
	}}
	require.NoError(t, bytecode.VerifyRegisters(fn))
	r := NewRealm()
	runScriptsIn(t, r, "globalThis.a = 1, globalThis.b = 2, globalThis.c = 3; delete globalThis.a; delete globalThis.b")
	require.Equal(t, uint8(2), r.globalRebuilds())
	f, _ := testClosure(r, fn)
	for _, this := range []*Object{r.Global, r.NewObject()} {
		v, err := r.CallObject(f, ObjectValue(this), nil)
		require.NoError(t, err)
		assert.Equal(t, 9.0, v.AsNumber())
	}
}

// runMutableProtoCases runs cases that modify intrinsics (frozen in a
// shared realm) in a mutable realm only.
func runMutableProtoCases(t *testing.T, cases []protoCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evalProtoBody(t, c.body, false)
			if len(c.want) > 0 && c.want[0] == '!' {
				assert.Truef(t, len(got) > 1 && got[0] == '!' && hasPrefix(got[1:], c.want[1:]), "got %q, want error %q", got, c.want[1:])
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// iterLib defines logging iterables for the protocol cases: iter(n, opts)
// yields 0..n-1 and logs next/return calls into log.
const iterLib = `
const log = [];
function iter(n, opts) {
	opts = opts || {};
	let i = 0;
	const it = {
		next() { log.push("next"); if (opts.nextThrows) throw new Error("next"); return i < n ? {value: i++, done: false} : {value: "end", done: true}; },
	};
	if (!opts.noReturn) {
		it.return = function (...args) {
			log.push("return:" + (this === it) + ":" + args.length);
			if (opts.returnThrows) throw new Error("return");
			return opts.returnPrimitive ? 1 : {};
		};
	}
	return {[Symbol.iterator]() { log.push("iter"); return it; }};
}
`

func TestIteratorProtocolForOf(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"user iterable", iterLib + `const out = []; for (const v of iter(3)) out.push(v); return out.join() + "|" + log.join()`, "0,1,2|iter,next,next,next,next"},
		{"break closes", iterLib + `for (const v of iter(3)) { if (v === 1) break; } return log.join()`, "iter,next,next,return:true:0"},
		{"return closes", iterLib + `function f() { for (const v of iter(3)) return v; } return f() + "|" + log.join()`, "0|iter,next,return:true:0"},
		{"bare return closes", iterLib + `function f() { for (const v of iter(3)) { if (v === 1) return; } } const g = () => { for (const a of iter(2)) for (const b of iter(2)) return; }; return [f(), g()].join() + "|" + log.join()`, ",|iter,next,next,return:true:0,iter,next,iter,next,return:true:0,return:true:0"},
		{"throw closes", iterLib + `try { for (const v of iter(3)) throw new Error("body"); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,body"},
		{"throw beats return error", iterLib + `try { for (const v of iter(3, {returnThrows: true})) throw new Error("body"); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,body"},
		{"throw ignores non-object", iterLib + `try { for (const v of iter(3, {returnPrimitive: true})) throw new Error("body"); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,body"},
		{"break return error", iterLib + `try { for (const v of iter(3, {returnThrows: true})) break; } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,return"},
		{"break non-object result", iterLib + `for (const v of iter(3, {returnPrimitive: true})) break;`, "!TypeError"},
		{"next throws no close", iterLib + `try { for (const v of iter(3, {nextThrows: true})); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,next"},
		{"exhausted no close", iterLib + `for (const v of iter(2)); return log.join()`, "iter,next,next,next"},
		{"continue keeps open", iterLib + `for (const v of iter(2)) continue; return log.join()`, "iter,next,next,next"},
		{"no return method", iterLib + `for (const v of iter(3, {noReturn: true})) break; return log.join()`, "iter,next"},
		{"binding throws closes", iterLib + `try { for (const {x = (() => { throw new Error("bind"); })()} of iter(2)); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,bind"},
		{"labeled nested", iterLib + `outer: for (const a of iter(2)) { for (const b of iter(2)) { log.push("body"); continue outer; } } return log.join()`,
			"iter,next,iter,next,body,return:true:0,next,iter,next,body,return:true:0,next"},
		{"labeled break outer", iterLib + `outer: for (const a of iter(2)) { for (const b of iter(2)) break outer; } return log.join()`, "iter,next,iter,next,return:true:0,return:true:0"},
		{"inner close throws closes outer", iterLib + `const bad = iter(2, {returnThrows: true}); try { outer: for (const a of iter(2)) { for (const b of bad) break outer; } } catch (e) { log.push(e.message); } return log.join()`,
			"iter,next,iter,next,return:true:0,return:true:0,return"},
		{"close error caught inside", iterLib + `outer: for (const a of iter(2)) { try { for (const b of iter(2, {returnThrows: true})) break outer; } catch (e) { log.push(e.message); } } return log.join()`,
			"iter,next,iter,next,return:true:0,return,next,iter,next,return:true:0,return,next"},
		{"break through finally", iterLib + `for (const v of iter(2)) { try { break; } finally { log.push("finally"); } } return log.join()`, "iter,next,finally,return:true:0"},
		{"break out of try", iterLib + `L: try { for (const v of iter(2)) break L; } finally { log.push("finally"); } return log.join()`, "iter,next,return:true:0,finally"},
		{"return through finally", iterLib + `function f() { for (const v of iter(2)) { try { return "r"; } finally { log.push("finally"); } } } return f() + "|" + log.join()`, "r|iter,next,finally,return:true:0"},
		{"return in try around loop", iterLib + `function f() { try { for (const v of iter(2)) return "r"; } finally { log.push("finally"); } } return f() + "|" + log.join()`, "r|iter,next,return:true:0,finally"},
		{"switch break keeps open", iterLib + `for (const v of iter(2)) { switch (v) { case 0: break; } log.push("after"); } return log.join()`, "iter,next,after,next,after,next"},
		{"labeled block", iterLib + `L: { for (const v of iter(2)) break L; } return log.join()`, "iter,next,return:true:0"},
		{"nested function return", iterLib + `for (const v of iter(1)) { (() => { return 1; })(); } return log.join()`, "iter,next,next"},
		{"not iterable", `for (const v of 1);`, "!TypeError: 1 is not iterable"},
		{"object not iterable", `for (const v of {});`, "!TypeError"},
		{"null not iterable", `for (const v of null);`, "!TypeError"},
		{"iterator not object", `for (const v of {[Symbol.iterator]() { return 1; }});`, "!TypeError"},
		{"next not callable", `for (const v of {[Symbol.iterator]() { return {next: 1}; }});`, "!TypeError"},
		{"result not object", `for (const v of {[Symbol.iterator]() { return {next() { return 1; }}; }});`, "!TypeError"},
		{"done getter", `let n = 0; const it = {next() { return {get done() { n++; return n > 2; }, value: n}; }}; const out = []; for (const v of {[Symbol.iterator]: () => it}) out.push(v); return out.join()`, "0,1"},
		{"next read once", `let reads = 0; const it = {get next() { reads++; let i = 0; return () => ({done: i++ > 2}); }}; for (const v of {[Symbol.iterator]: () => it}); return reads`, "1"},
		{"array live length", `const a = [1]; const out = []; for (const v of a) { out.push(v); if (a.length < 3) a.push(v + 1); } return out.join()`, "1,2,3"},
		{"array holes", `const out = []; for (const v of [, 1]) out.push(v); return out.join("|")`, "|1"},
		{"array iterator object", `const it = [1, 2][Symbol.iterator](); it.next(); const out = []; for (const v of it) out.push(v); return out.join()`, "2"},
		{"own iterator on array", `const a = [1, 2]; a[Symbol.iterator] = () => ["x"][Symbol.iterator](); const out = []; for (const v of a) out.push(v); return out.join()`, "x"},
		{"string code points", `const out = []; for (const c of "a\u{1F600}b\uD800") out.push(c.length); return out.join()`, "1,2,1,1"},
		{"entries", `const out = []; for (const [i, v] of ["a", "b"].entries()) out.push(i + v); return out.join()`, "0a,1b"},
	})
}

func TestIteratorProtocolDestructuringAndSpread(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"pattern closes", iterLib + `const [a] = iter(3); return a + "|" + log.join()`, "0|iter,next,return:true:0"},
		{"pattern exhausted", iterLib + `const [a, b, c] = iter(2); return [a, b, c] + "|" + log.join()`, "0,1,|iter,next,next,next"},
		{"pattern rest", iterLib + `const [a, ...r] = iter(3); return r + "|" + log.join()`, "1,2|iter,next,next,next,next"},
		{"pattern elision", iterLib + `const [, b] = iter(3); return b + "|" + log.join()`, "1|iter,next,next,return:true:0"},
		{"empty pattern", iterLib + `const [] = iter(3); return log.join()`, "iter,return:true:0"},
		{"pattern default throws", iterLib + `try { const [a = (() => { throw new Error("d"); })()] = iter(0); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,d"},
		{"pattern target throws", iterLib + `try { const [[x]] = iter(3); } catch (e) { log.push(e.name); } return log.join()`, "iter,next,return:true:0,TypeError"},
		{"assignment target throws", iterLib + `const o = {set p(v) { throw new Error("set"); }}; try { [o.p] = iter(3); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,set"},
		{"pattern next throws", iterLib + `try { const [a] = iter(3, {nextThrows: true}); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,next"},
		{"pattern close error", iterLib + `try { const [a] = iter(3, {returnThrows: true}); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,return"},
		{"pattern close non-object", iterLib + `const [a] = iter(3, {returnPrimitive: true});`, "!TypeError"},
		{"param pattern", iterLib + `function f([a, b]) { return a + b; } return f(iter(5)) + "|" + log.join()`, "1|iter,next,next,return:true:0"},
		{"string pattern", `const [a, b, ...c] = "x\u{1F600}yz"; return [a, b.length, c.join("")].join()`, "x,2,yz"},
		{"array pattern", `const [a, , b = 5, ...c] = [1, 2, undefined, 4, 5]; return [a, b, c.join("")].join()`, "1,5,45"},
		{"spread array", iterLib + `return [0, ...iter(2), 9].join() + "|" + log.join()`, "0,0,1,9|iter,next,next,next"},
		{"spread call", iterLib + `return Math.max(...iter(3)) + "|" + log.join()`, "2|iter,next,next,next,next"},
		{"spread new", iterLib + `function F(a, b) { this.s = a + b; } return new F(...iter(3)).s`, "1"},
		{"spread string", `return [..."a\u{1F600}"].length`, "2"},
		{"spread not iterable", `return [...{}]`, "!TypeError"},
		{"spread next throws", iterLib + `try { [...iter(2, {nextThrows: true})]; } catch (e) { log.push(e.message); } return log.join()`, "iter,next,next"},
		{"spread own iterator", `const a = [1, 2]; a[Symbol.iterator] = () => ["z"][Symbol.iterator](); return [...a].join() + Math.max(...a)`, "zNaN"},
	})
}

func TestIteratorProtocolBuiltins(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"Array.from iterable", iterLib + `return Array.from(iter(3), v => v * 2).join() + "|" + log.join()`, "0,2,4|iter,next,next,next,next"},
		{"Array.from map throws", iterLib + `try { Array.from(iter(3), v => { throw new Error("map"); }); } catch (e) { log.push(e.message); } return log.join()`, "iter,next,return:true:0,map"},
		{"Array.from string", `return Array.from("a\u{1F600}").length`, "2"},
		{"Array.from array-like", `return Array.from({length: 2, 0: "a"}).join("|")`, "a|"},
		{"Array.from iterator not callable", `return Array.from({[Symbol.iterator]: 1})`, "!TypeError"},
		{"Array.from iterator null", `return Array.from({[Symbol.iterator]: null, length: 1, 0: 7}).join()`, "7"},
		{"Array.from ctor", iterLib + `function C() { this.made = true; } const r = Array.from.call(C, iter(2)); return [r.made, r.length, r[1]].join()`, "true,2,1"},
		{"Array.from own iterator on array", `const a = [1]; a[Symbol.iterator] = () => ["q"][Symbol.iterator](); return Array.from(a).join()`, "q"},
		{"fromEntries iterable", `const m = [["a", 1], ["b", 2]][Symbol.iterator](); return JSON.stringify(Object.fromEntries(m))`, `{"a":1,"b":2}`},
		{"fromEntries bad entry closes", iterLib + `const src = {[Symbol.iterator]() { const it = iter(2)[Symbol.iterator](); return {next() { const r = it.next(); r.value = r.value === 0 ? 5 : r.value; return r; }, return: it.return}; }}; try { Object.fromEntries(src); } catch (e) { log.push(e.name); } return log.join()`,
			"iter,next,return:false:0,TypeError"},
		{"fromEntries key throws closes", iterLib + `const bad = {toString() { throw new Error("key"); }}; const src = {[Symbol.iterator]() { let d = false; return {next() { const r = {value: [bad, 1], done: d}; d = true; return r; }, return() { log.push("closed"); return {}; }}; }}; try { Object.fromEntries(src); } catch (e) { log.push(e.message); } return log.join()`,
			"closed,key"},
		{"fromEntries not iterable", `return Object.fromEntries({})`, "!TypeError"},
	})
}

func TestIteratorPrototypes(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"array @@iterator is values", `return Array.prototype[Symbol.iterator] === Array.prototype.values && Array.prototype.values.name`, "values"},
		{"iterator prototype", `const IP = Object.getPrototypeOf(Object.getPrototypeOf([][Symbol.iterator]())); return [IP[Symbol.iterator]() === IP, IP[Symbol.iterator].name, Object.getPrototypeOf(IP) === Object.prototype].join()`, "true,[Symbol.iterator],true"},
		{"string iterator prototype", `const it = ""[Symbol.iterator](); const SP = Object.getPrototypeOf(it); return [Object.prototype.toString.call(it), SP[Symbol.toStringTag], Object.getPrototypeOf(SP) === Object.getPrototypeOf(Object.getPrototypeOf([].values())), typeof SP.next, SP.next.length].join()`,
			"[object String Iterator],String Iterator,true,function,0"},
		{"string iterator next", `const it = "a\u{1F600}"[Symbol.iterator](); return JSON.stringify([it.next(), it.next().value.length, it.next(), it.next()])`, `[{"value":"a","done":false},2,{"done":true},{"done":true}]`},
		{"string iterator coerces", `return [...String.prototype[Symbol.iterator].call(12)].join()`, "1,2"},
		{"string iterator nullish", `return String.prototype[Symbol.iterator].call(null)`, "!TypeError"},
		{"string iterator name", `return String.prototype[Symbol.iterator].name + String.prototype[Symbol.iterator].length`, "[Symbol.iterator]0"},
		{"string next receiver", `return ""[Symbol.iterator]().next.call([].values())`, "!TypeError"},
		{"unscopables", `const u = Array.prototype[Symbol.unscopables]; return [Object.getPrototypeOf(u), Object.keys(u).join(" ")].join()`,
			",at copyWithin entries fill find findIndex findLast findLastIndex flat flatMap includes keys toReversed toSorted toSpliced values"},
	})
	// Attributes as installed (a shared realm freezes every intrinsic).
	runMutableProtoCases(t, []protoCase{
		{"array iterator toStringTag", `const d = Object.getOwnPropertyDescriptor(Object.getPrototypeOf([].values()), Symbol.toStringTag); return [d.value, d.writable, d.enumerable, d.configurable].join()`, "Array Iterator,false,false,true"},
		{"unscopables attrs", `const u = Array.prototype[Symbol.unscopables]; const d = Object.getOwnPropertyDescriptor(Array.prototype, Symbol.unscopables); const e = Object.getOwnPropertyDescriptor(u, "at"); return [d.writable, d.enumerable, d.configurable, e.writable, e.enumerable, e.configurable].join()`,
			"false,false,true,true,true,true"},
		{"array @@iterator attrs", `const d = Object.getOwnPropertyDescriptor(Array.prototype, Symbol.iterator); return [d.writable, d.enumerable, d.configurable].join()`, "true,false,true"},
	})
}

// TestIteratorProtocolModifiedIntrinsics covers the fast-path guards: once a
// script replaces an iteration method, every consumer observes it, and
// restoring it brings the original behaviour back.
func TestIteratorProtocolModifiedIntrinsics(t *testing.T) {
	const all = `function all(x) { const out = []; for (const v of x) out.push(v); const [d] = x; return [out.join(""), [...x].join(""), d, Array.from(x).join(""), Math.max.apply(null, [0].concat(Array.from(x).map(Number).filter(n => n === n)))].join("|"); }`
	runMutableProtoCases(t, []protoCase{
		{"array @@iterator", all + `const a = [1, 2]; const orig = Array.prototype[Symbol.iterator]; Array.prototype[Symbol.iterator] = function () { return ["x", "y"].values(); }; const r = all(a); Array.prototype[Symbol.iterator] = orig; return r + "#" + all(a)`,
			"xy|xy|x|xy|0#12|12|1|12|2"},
		{"array iterator next", all + `const AIP = Object.getPrototypeOf([].values()); const orig = AIP.next; let n = 0; AIP.next = function () { return n++ < 1 ? {value: "n", done: false} : {done: true}; }; const r = all([1, 2]); AIP.next = orig; return r + "#" + all([3])`,
			"n||||0#3|3|3|3|3"},
		{"array iterator return", `const AIP = Object.getPrototypeOf([].values()); const log = []; AIP.return = function () { log.push("ret" + (this instanceof Object)); return {}; }; for (const v of [1, 2]) break; const [a] = [1, 2]; const [b, c] = [1]; for (const v of "ab") break; delete AIP.return; for (const v of [1]) break; return log.join()`,
			"rettrue,rettrue"},
		{"iterator prototype return", `const IP = Object.getPrototypeOf(Object.getPrototypeOf([].values())); const log = []; IP.return = function () { log.push(Object.prototype.toString.call(this)); return {}; }; for (const v of [1, 2]) break; for (const v of "ab") break; delete IP.return; return log.join()`,
			"[object Array Iterator],[object String Iterator]"},
		{"object prototype return", `const log = []; Object.prototype.return = function () { log.push("op"); return {}; }; for (const v of [1, 2]) break; delete Object.prototype.return; return log.join()`, "op"},
		{"return sees position", `const AIP = Object.getPrototypeOf([].values()); let rest; AIP.return = function () { rest = [...this]; return {}; }; for (const v of [1, 2, 3]) break; delete AIP.return; return rest.join()`, "2,3"},
		{"string @@iterator", `const orig = String.prototype[Symbol.iterator]; String.prototype[Symbol.iterator] = function () { return ["s"].values(); }; const r = [..."abc"].join() + Array.from("xy").join(); String.prototype[Symbol.iterator] = orig; return r + "#" + [..."ab"].join()`,
			"ss#a,b"},
		{"string iterator next", `const SIP = Object.getPrototypeOf(""[Symbol.iterator]()); const orig = SIP.next; SIP.next = () => ({done: true}); const r = [..."abc"].length; SIP.next = orig; return r + "#" + [..."abc"].length`, "0#3"},
		{"values replaced keeps @@iterator", `const orig = Array.prototype.values; Array.prototype.values = function () { return ["v"].values(); }; const r = [...[1, 2]].join(); Array.prototype.values = orig; return r`, "1,2"},
		{"array proto accessor iterator", `const orig = Array.prototype[Symbol.iterator]; Object.defineProperty(Array.prototype, Symbol.iterator, {get() { return () => ["g"].values(); }, configurable: true}); const r = [...[1]].join(); Object.defineProperty(Array.prototype, Symbol.iterator, {value: orig, writable: true, configurable: true}); return r + [...[1]].join()`, "g1"},
		{"array subclass proto", `const P = Object.create(Array.prototype); P[Symbol.iterator] = function () { return ["p"].values(); }; const a = [1]; Object.setPrototypeOf(a, P); return [...a].join()`, "p"},
		{"getter element", `const a = [1, 2]; Object.defineProperty(a, 1, {get() { return "g"; }}); return [...a].join()`, "1,g"},
		{"inherited element", `Array.prototype[1] = "h"; const r = [...[1, , 3]].join(); delete Array.prototype[1]; return r`, "1,h,3"},
	})
}

func TestIteratorCloseInterrupt(t *testing.T) {
	f := evalModule(t, `
export function run() {
	const it = {next() { return {done: false}; }, return() { mark(); return {}; }};
	for (const v of {[Symbol.iterator]: () => it}) stop();
}`)
	r := f.r
	called := false
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("stop"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		r.Interrupt("halt")
		return Undefined(), r.CheckInterrupt()
	}))))
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("mark"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		called = true
		return Undefined(), nil
	}))))
	_, err := f.callErr("run")
	var ie *InterruptedError
	assert.ErrorAs(t, err, &ie)
	assert.False(t, called, "an interrupt skips the return method")
}

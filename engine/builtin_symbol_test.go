package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// protoCase is one iterator-protocol semantics case: body is a function body
// (module code, so strict) whose return value, rendered by ToGo and %v, must
// equal want. A want starting with "!" expects a thrown error whose display
// string ("Name: message") has the rest as prefix.
type protoCase struct {
	name string
	body string
	want string
}

// runProtoCases runs every case in a mutable and in a shared realm.
func runProtoCases(t *testing.T, cases []protoCase) {
	t.Helper()
	for _, shared := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/shared=%v", c.name, shared), func(t *testing.T) {
				got := evalProtoBody(t, c.body, shared)
				if len(c.want) > 0 && c.want[0] == '!' {
					assert.Truef(t, len(got) > 1 && got[0] == '!' && hasPrefix(got[1:], c.want[1:]), "got %q, want error %q", got, c.want[1:])
					return
				}
				assert.Equal(t, c.want, got)
			})
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// evalProtoBody evaluates body as a function in a fresh realm; a thrown
// error is returned as "!" + its display string.
func evalProtoBody(t *testing.T, body string, shared bool) string {
	t.Helper()
	m, err := syntax.ParseModule("t.js", "export function f() {\n"+body+"\n}", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	res, err := r.Call(fn, Undefined(), nil)
	if err != nil {
		var exc *Exception
		if ok := asException(err, &exc); ok {
			return "!" + errorDisplayString(exc.Value)
		}
		return "!" + err.Error()
	}
	return fmt.Sprintf("%v", r.ToGo(res))
}

func asException(err error, exc **Exception) bool {
	e, ok := err.(*Exception)
	if ok {
		*exc = e
	}
	return ok
}

func TestSymbolSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"typeof", `return typeof Symbol() + " " + typeof Symbol.iterator + " " + typeof Object(Symbol())`, "symbol symbol object"},
		{"description", `return [Symbol("x").description, Symbol().description === undefined, Symbol("").description].join("|")`, "x|true|"},
		{"toString", `return String(Symbol("a")) + Symbol("b").toString() + String(Symbol())`, "Symbol(a)Symbol(b)Symbol()"},
		{"concat throws", `return Symbol() + ""`, "!TypeError: Cannot convert a Symbol value to a string"},
		{"template throws", "return `${Symbol()}`", "!TypeError"},
		{"number throws", `return +Symbol()`, "!TypeError"},
		{"new throws", `return new Symbol()`, "!TypeError"},
		{"unique", `return Symbol("a") === Symbol("a")`, "false"},
		{"for", `return Symbol.for("k") === Symbol.for("k") && Symbol.keyFor(Symbol.for("k")) === "k" && Symbol.keyFor(Symbol("k")) === undefined`, "true"},
		{"keyFor non-symbol", `return Symbol.keyFor("x")`, "!TypeError"},
		{"well-known", `return [Symbol.iterator, Symbol.asyncIterator, Symbol.hasInstance, Symbol.isConcatSpreadable, Symbol.match, Symbol.matchAll, Symbol.replace, Symbol.search, Symbol.species, Symbol.split, Symbol.toPrimitive, Symbol.toStringTag, Symbol.unscopables].map(s => s.description).join()`,
			"Symbol.iterator,Symbol.asyncIterator,Symbol.hasInstance,Symbol.isConcatSpreadable,Symbol.match,Symbol.matchAll,Symbol.replace,Symbol.search,Symbol.species,Symbol.split,Symbol.toPrimitive,Symbol.toStringTag,Symbol.unscopables"},
		{"well-known attrs", `const d = Object.getOwnPropertyDescriptor(Symbol, "iterator"); return [d.writable, d.enumerable, d.configurable].join()`, "false,false,false"},
		{"property key", `const s = Symbol("k"); const o = {[s]: 1, a: 2}; o[s]++; return o[s] + "," + Object.keys(o) + "," + JSON.stringify(o) + "," + Object.getOwnPropertySymbols(o).length`, `2,a,{"a":2},1`},
		{"own keys order", `const s = Symbol(); const o = {}; o[s] = 1; o.b = 2; o[1] = 3; return Object.getOwnPropertyNames(o).join() + "|" + (Object.getOwnPropertySymbols(o)[0] === s)`, "1,b|true"},
		{"assign copies symbols", `const s = Symbol(); const t = Object.assign({}, {[s]: 7, x: 1}); return t[s] + t.x`, "8"},
		{"spread copies symbols", `const s = Symbol(); const t = {...{[s]: 7}}; return t[s]`, "7"},
		{"json value", `return JSON.stringify({a: Symbol(), b: [Symbol()]})`, `{"b":[null]}`},
		{"wrapper", `const s = Symbol("w"); const o = Object(s); return [typeof o, o.valueOf() === s, o.description, o instanceof Symbol, Object.getPrototypeOf(o) === Symbol.prototype].join()`, "object,true,w,true,true"},
		{"primitive methods", `return Symbol("p").toString() + Symbol.prototype.toString.call(Symbol("q"))`, "Symbol(p)Symbol(q)"},
		{"proto toStringTag", `return Object.prototype.toString.call(Symbol()) + Symbol.prototype[Symbol.toStringTag]`, "[object Symbol]Symbol"},
		{"toPrimitive", `const s = Symbol(); return Symbol.prototype[Symbol.toPrimitive].call(Object(s)) === s && Symbol.prototype[Symbol.toPrimitive].name`, "[Symbol.toPrimitive]"},
		{"this check", `return Symbol.prototype.valueOf.call({})`, "!TypeError"},
		{"description getter this", `return Object.getOwnPropertyDescriptor(Symbol.prototype, "description").get.call(1)`, "!TypeError"},
		{"symbol on primitive set", `"use strict"; const s = Symbol(); s.x = 1`, "!TypeError"},
		{"symbol lengths", `return Symbol.length + "," + Symbol.for.length + "," + Symbol.keyFor.length`, "0,1,1"},
		{"getOwnPropertySymbols of primitive", `return Object.getOwnPropertySymbols("ab").length`, "0"},
	})
}

func TestToPrimitiveProtocol(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"hint", `const seen = []; const o = {[Symbol.toPrimitive](h) { seen.push(h); return 1; }}; +o; o + ""; String(o); ` + "`${o}`" + `; o == 1; return seen.join()`, "number,default,string,string,default"},
		{"object result", `const o = {[Symbol.toPrimitive]() { return {}; }}; return o + 1`, "!TypeError"},
		{"not callable", `const o = {[Symbol.toPrimitive]: 1}; return o + 1`, "!TypeError"},
		{"null method falls back", `const o = {[Symbol.toPrimitive]: null, valueOf() { return 5; }}; return o + 1`, "6"},
		{"inherited", `function A() {}; A.prototype[Symbol.toPrimitive] = function () { return "a"; }; return new A() + "b"`, "ab"},
		{"getter", `let n = 0; const o = {get [Symbol.toPrimitive]() { n++; return () => 2; }}; return (o * 3) + n`, "7"},
		{"property key", `const o = {[Symbol.toPrimitive]() { return "k"; }}; const t = {k: 9}; return t[o]`, "9"},
		{"date default", `const d = new Date(0); return typeof (d + 1)`, "string"},
		{"date method", `const f = Date.prototype[Symbol.toPrimitive]; const d = Object.getOwnPropertyDescriptor(Date.prototype, Symbol.toPrimitive); return [new Date(0)[Symbol.toPrimitive] === f, typeof f, f.name, f.length, d.writable, d.enumerable].join()`, "true,function,[Symbol.toPrimitive],1,false,false"},
		{"date hints", `const d = new Date(5); return [+d, d - 1, typeof ` + "`${d}`" + `, d[Symbol.toPrimitive]("number"), typeof d[Symbol.toPrimitive]("default"), String(new Date(NaN))].join()`, "5,4,string,5,string,Invalid Date"},
		{"date bad hint", `return new Date(0)[Symbol.toPrimitive]("x")`, "!TypeError"},
		{"date first touch", `const d = new Date(7); const own = Object.getOwnPropertySymbols(Date.prototype); return [d * 1, own.length, own[0] === Symbol.toPrimitive].join()`, "7,1,true"},
		{"date own override", `const d = new Date(0); Object.defineProperty(d, Symbol.toPrimitive, {value: () => 42}); return d + 1`, "43"},
		{"array", `return [1, 2] + ""`, "1,2"},
	})
}

func TestToStringTagProtocol(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"own", `return Object.prototype.toString.call({[Symbol.toStringTag]: "X"})`, "[object X]"},
		{"non-string ignored", `return Object.prototype.toString.call({[Symbol.toStringTag]: 1})`, "[object Object]"},
		{"overrides builtin", `const a = []; a[Symbol.toStringTag] = "Y"; return Object.prototype.toString.call(a)`, "[object Y]"},
		{"inherited", `const p = {get [Symbol.toStringTag]() { return "C"; }}; return String(Object.create(p))`, "[object C]"},
		{"builtins", `return [Object.prototype.toString.call([]), Object.prototype.toString.call(() => 1), Object.prototype.toString.call(new Error()), Object.prototype.toString.call(null), Object.prototype.toString.call(1)].join()`,
			"[object Array],[object Function],[object Error],[object Null],[object Number]"},
		{"namespace tags", `return [Math, JSON, Reflect].map(o => Object.prototype.toString.call(o) + ":" + Object.getOwnPropertyDescriptor(o, Symbol.toStringTag).enumerable).join()`,
			"[object Math]:false,[object JSON]:false,[object Reflect]:false"},
	})
}

func TestHasInstanceProtocol(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"custom", `const Even = {[Symbol.hasInstance](n) { return n % 2 === 0; }}; return (2 instanceof Even) + "," + (3 instanceof Even)`, "true,false"},
		{"own on function", `function C() {}; Object.defineProperty(C, Symbol.hasInstance, {value: v => v === 1}); return (1 instanceof C) + "," + (new C() instanceof C)`, "true,false"},
		{"truthy coerced", `const o = {[Symbol.hasInstance]() { return "yes"; }}; return 0 instanceof o`, "true"},
		{"non-callable", `const o = {[Symbol.hasInstance]: 1}; return 0 instanceof o`, "!TypeError"},
		{"object without method", `return 0 instanceof {}`, "!TypeError"},
		{"ordinary", `function F() {}; return (new F() instanceof F) + "," + ([] instanceof Array) + "," + ([] instanceof Object)`, "true,true,true"},
		{"function proto method", `function F() {}; const f = new F(); return Function.prototype[Symbol.hasInstance].call(F, f) + "," + Function.prototype[Symbol.hasInstance].call({}, f)`, "true,false"},
		{"function proto attrs", `const d = Object.getOwnPropertyDescriptor(Function.prototype, Symbol.hasInstance); return [d.writable, d.enumerable, d.configurable, d.value.name].join()`, "false,false,false,[Symbol.hasInstance]"},
		{"bound", `function F() {}; const B = F.bind(null); return new F() instanceof B`, "true"},
	})
}

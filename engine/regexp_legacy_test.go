package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegExpCompile covers RegExp.prototype.compile (B.2.4.1) in both kinds
// of realm, and the paths that hold a RegExp's payload while user code
// that may recompile it runs (staging/sm/RegExp and annexB's
// Symbol.split).
func TestRegExpCompile(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"descriptor", `const d = Object.getOwnPropertyDescriptor(RegExp.prototype, "compile");
			return [typeof d.value, d.writable === d.configurable, d.enumerable, d.value.length, d.value.name].join()`,
			"function,true,false,2,compile"},
		{"recompiles in place", `const re = /a/g; re.lastIndex = 3; const r = re.compile("b+", "i");
			return [r === re, re.source, re.flags, re.lastIndex, re.test("xBB"), re.test("a")].join()`,
			"true,b+,i,0,true,false"},
		{"pattern RegExp", `const re = /a/; re.compile(/x(y)/gi); return [re.source, re.flags, re.exec("XY")[1]].join()`,
			"x(y),gi,Y"},
		{"pattern RegExp itself", `const re = /a/m; re.compile(re); return [re.source, re.flags].join()`, "a,m"},
		{"pattern RegExp and flags", `/a/.compile(/x/, "g")`, "!TypeError"},
		{"undefined", `const re = /a/g; re.compile(); return [re.source, re.flags, re.test("")].join()`, "(?:),,true"},
		{"coerced", `const re = /a/; re.compile({toString() { return "b" }}, {toString() { return "y" }}); return [re.source, re.flags].join()`,
			"b,y"},
		{"flags order", `const re = /(?:)/; re.compile("(?:)", "imsuyg"); return re.flags`, "gimsuy"},
		{"invalid flags leave it", `const re = /a/g; try { re.compile("b", "gg"); } catch (e) { return [e.name, re.source, re.flags].join(); }`,
			"SyntaxError,a,g"},
		{"invalid pattern leaves it", `const re = /a/g; try { re.compile("("); } catch (e) { return [e.name, re.source, re.flags].join(); }`,
			"SyntaxError,a,g"},
		{"u pattern", `const re = /a/; try { re.compile("\\-", "u"); } catch (e) { return e.name; }`, "SyntaxError"},
		{"RegExp called", `const re = RegExp("a"); re.compile("b"); return re.source`, "b"},
		{"new RegExp", `const re = new RegExp("a"); re.compile("b"); return re.source`, "b"},
		{"String.prototype.match's RegExp", `let re; "a".match({[Symbol.match]: undefined, toString() { return "a"; }});
			re = RegExp.prototype[Symbol.matchAll].call(/a/g, "a").next().value; return re[0]`, "a"},
		{"subclass instance", `class R extends RegExp {} const re = new R("a");
			try { re.compile("b"); } catch (e) { return [e.name, re.source].join(); }`, "TypeError,a"},
		{"other newTarget", `const re = Reflect.construct(RegExp, ["a"], Object);
			try { RegExp.prototype.compile.call(re, "b"); } catch (e) { return [e.name, RegExp.prototype.source.call === undefined].join(); }`,
			"TypeError,true"},
		{"not a RegExp", `RegExp.prototype.compile.call({})`, "!TypeError"},
		{"the prototype", `RegExp.prototype.compile.call(RegExp.prototype)`, "!TypeError"},
		{"primitive", `RegExp.prototype.compile.call("a")`, "!TypeError"},
		{"non-writable lastIndex", `const re = /foo/i; Object.defineProperty(re, "lastIndex", {value: 42, writable: false});
			try { re.compile("bar"); } catch (e) { return [e.name, re.source, re.flags, re.lastIndex, re.test("bar"), re.test("BAR")].join(); }`,
			"TypeError,bar,,42,true,false"},

		// RegExpBuiltinExec reads the flags and matcher after lastIndex.
		{"exec: valueOf recompiles", `const re = /a/; re.lastIndex = {valueOf() { re.compile("b", "g"); return 0; }};
			const m = re.exec("ab"); return [m[0], m.index, re.lastIndex].join()`, "b,1,2"},
		{"test: valueOf recompiles", `const out = [];
			for (const flag of ["", "y", "g"]) {
				const re = new RegExp("a", flag); re.lastIndex = {valueOf() { re.compile("b"); return 0; }};
				out.push(re.test("b"));
			}
			return out.join()`, "true,true,true"},
		{"test: valueOf adds g", `const re = /a/; re.lastIndex = {valueOf() { re.compile("a", "g"); return 0; }};
			return [re.test("aa"), re.lastIndex].join()`, "true,1"},
		{"test: simple class", `const re = /\s/; re.lastIndex = {valueOf() { re.compile("x"); return 0; }}; return re.test("a b")`, "false"},
		{"match: valueOf recompiles", `const out = [];
			for (const flag of ["", "y"]) {
				const re = new RegExp("a", flag); re.lastIndex = {valueOf() { re.compile("b"); return 0; }};
				out.push(re[Symbol.match]("b") !== null);
			}
			return out.join()`, "true,true"},
		{"match: valueOf adds g", `const re = /a/; re.lastIndex = {valueOf() { re.compile("a", "g"); return 0; }};
			re[Symbol.match]("a"); return re.lastIndex`, "1"},
		{"match: valueOf drops y", `const re = /a/y; re.lastIndex = {valueOf() { re.compile("a", ""); re.lastIndex = 9000; return 0; }};
			re[Symbol.match]("a"); return re.lastIndex`, "9000"},
		{"match: groups of the new program", `const re = /(?<x>a)/; re.lastIndex = {valueOf() { re.compile("(?<y>b)"); return 0; }};
			const m = "ab".match(re); return [m[0], m.groups.y, "x" in m.groups].join()`, "b,b,false"},
		{"replace: valueOf recompiles", `const out = [];
			for (const flag of ["", "y"]) {
				const re = new RegExp("a", flag); re.lastIndex = {valueOf() { re.compile("b"); return 0; }};
				out.push(re[Symbol.replace]("b", "pass"));
			}
			return out.join()`, "pass,pass"},
		{"replace: valueOf adds g", `const re = /a/; re.lastIndex = {valueOf() { re.compile("a", "g"); return 0; }};
			return [re[Symbol.replace]("aa", "_"), re.lastIndex].join()`, "_a,1"},
		{"replace: valueOf drops y", `const re = /a/y; re.lastIndex = {valueOf() { re.compile("a", ""); re.lastIndex = 9000; return 0; }};
			re[Symbol.replace]("a", ""); return re.lastIndex`, "9000"},
		{"replace: simple class", `const re = /\s/; re.lastIndex = {valueOf() { re.compile("x"); return 0; }}; return "a x".replace(re, "_")`, "a _"},

		// The spec finds every match before the first replacer call.
		{"replacer recompiles", `const rx = RegExp("a", "g");
			const a = rx[Symbol.replace]("abba", () => { rx.compile("b", "g"); return "?"; });
			const rx2 = RegExp("a", "g");
			const b = "abba".replace(rx2, () => { rx2.compile("b", "g"); return "?"; });
			return [a, b].join()`, "?bb?,?bb?"},
		{"replacer recompiles: simple class", `const rx = /\s/g; return " a b".replace(rx, () => { rx.compile("a", "g"); return "_"; })`, "_a_b"},
		{"replacer recompiles: elem base", `const rx = /a/g; const b = {get a() { rx.compile("b"); return "A"; }};
			return rx[Symbol.replace]("aaa", a => b[a])`, "AAA"},
		{"replacer recompiles: groups", `const rx = /(?<x>a)/g;
			return "aa".replace(rx, (...args) => { const g = args[args.length - 1]; rx.compile("(?<y>b)", "g"); return g.x; })`, "aa"},
		{"replacer recompiles: captures", `const rx = /(a)(b)?/g;
			return "aab".replace(rx, (m, p1, p2) => { rx.compile("(c)", "g"); return "[" + p1 + (p2 ?? "-") + "]"; })`, "[a-][ab]"},

		// The splitter and matcher are made before the limit and lastIndex.
		{"split: @@match recompiles", `const re = /a/; Object.defineProperty(re, Symbol.match, {get() { re.compile("b"); }});
			return JSON.stringify(re[Symbol.split]("abba"))`, `["a","","a"]`},
		{"split: limit recompiles", `const re = /a/; const limit = {valueOf() { re.compile("b"); return -1; }};
			return JSON.stringify(re[Symbol.split]("abba", limit))`, `["","bb",""]`},
		{"matchAll: lastIndex recompiles", `const re = /a/g; re.lastIndex = {valueOf() { re.compile("b", "g"); return 0; }};
			return [...re[Symbol.matchAll]("ab")].map(m => m[0]).join()`, "a"},
	})
}

// TestRegExpCompileMutable covers the pending compile of a mutable realm's
// %RegExp.prototype%: every way to observe the prototype defines it where
// the shared template has it.
func TestRegExpCompileMutable(t *testing.T) {
	runMutableProtoCases(t, []protoCase{
		{"descriptor", `const d = Object.getOwnPropertyDescriptor(RegExp.prototype, "compile"); return [d.writable, d.configurable].join()`,
			"true,true"},
		{"write first", `RegExp.prototype.foo = 1; return Object.getOwnPropertyNames(RegExp.prototype).slice(-2).join()`, "compile,foo"},
		{"define first", `Object.defineProperty(RegExp.prototype, "foo", {value: 1});
			return Object.getOwnPropertyNames(RegExp.prototype).slice(-2).join()`, "compile,foo"},
		{"delete first", `delete RegExp.prototype.exec; return [typeof RegExp.prototype.compile, "exec" in RegExp.prototype].join()`, "function,false"},
		{"delete it", `delete RegExp.prototype.compile; return typeof /a/.compile`, "undefined"},
		{"replace it", `RegExp.prototype.compile = 1; return /a/.compile`, "1"},
		{"has", `return "compile" in /a/`, "true"},
		{"own descriptor", `return Object.getOwnPropertyDescriptor(RegExp.prototype, "compile").value.name`, "compile"},
		{"hasOwn", `return Object.hasOwn(RegExp.prototype, "compile")`, "true"},
		{"shadows Object.prototype", `Object.prototype.compile = 1; return typeof /a/.compile`, "function"},
		{"cache filled before", `Object.prototype.compile = 1; const get = o => o.compile;
			get({}); get({}); return [get({}), typeof get(/a/), typeof get(/b/)].join()`, "1,function,function"},
		{"prevent extensions", `Object.preventExtensions(RegExp.prototype); return typeof RegExp.prototype.compile`, "function"},
		{"freeze", `Object.freeze(RegExp.prototype); return Object.isFrozen(RegExp.prototype) && typeof /a/.compile`, "function"},
		{"new prototype", `Object.setPrototypeOf(RegExp.prototype, null); return typeof /a/.compile`, "function"},
		{"pristine paths", `const s = "a-b".replace(/-/, "+") + "a-b".split(/-/).join("+") + /b/.test("b");
			/x/.compile("y"); return s + "a-b".replace(/-/, "+") + "a-b".split(/-/).join("+") + /b/.test("b")`, "a+ba+btruea+ba+btrue"},
		{"String conversions", `return [String(/a/), "" + /b/g, Object.prototype.toString.call(/c/), typeof RegExp.prototype.compile].join()`,
			"/a/,/b/g,[object RegExp],function"},
	})
}

// TestRegExpCompileKeyOrder: a mutable realm lists %RegExp.prototype%'s keys
// in the shared template's order, however the pending compile is defined.
func TestRegExpCompileKeyOrder(t *testing.T) {
	keys := `return Reflect.ownKeys(RegExp.prototype).map(String).join()`
	want := evalProtoBody(t, keys, true)
	assert.Contains(t, want, "hasIndices,compile,Symbol(Symbol.match)")
	assert.Equal(t, want, evalProtoBody(t, keys, false), "listed first")
	assert.Equal(t, want, evalProtoBody(t, `/a/.compile; `+keys, false), "looked up first")
	assert.Equal(t, want, evalProtoBody(t, `/a/.exec("a"); "a".replace(/a/, "b"); `+keys, false), "after the pristine paths")
}

// TestRegExpCompileLazy: a mutable realm defines compile on first use only,
// keeping the pristine paths meanwhile; a shared realm has it from the
// template.
func TestRegExpCompileLazy(t *testing.T) {
	r := NewRealm()
	p := r.RegExpPrototype
	require.IsType(t, (*pendingCompile)(nil), p.internal)
	assert.Equal(t, noFillAll, p.shape.noFill)
	rx, err := r.NewRegExp(FromGoString("a"), AtomEmpty)
	require.NoError(t, err)
	assert.NotNil(t, r.pristineRegExp(rx, rxExec|rxFlags|rxMatch|rxReplace), "a pending compile keeps the pristine paths")
	assert.True(t, r.lacksWellKnown(rx, SymbolKey(SymToPrimitive)))
	_, _, ok := p.shape.Lookup(compileKey)
	assert.False(t, ok)

	v, err := rx.GetProp(r, compileKey)
	require.NoError(t, err)
	require.True(t, IsCallable(v))
	assert.Nil(t, p.internal)
	assert.Zero(t, p.flags&flagHasLazy)
	assert.NotNil(t, r.pristineRegExp(rx, rxExec|rxFlags|rxMatch|rxReplace))
	_, err = r.Call(v, ObjectValue(rx), []Value{StringValue(FromGoString("b")), StringValue(FromGoString("g"))})
	require.NoError(t, err)
	assert.Equal(t, "b", rx.RegExpData().Source().GoString())
	assert.NotNil(t, r.pristineRegExp(rx, rxExec|rxFlags|rxMatch|rxReplace), "compile keeps the instance shape")

	s := newShared()
	assert.Nil(t, s.RegExpPrototype.internal)
	_, _, ok = s.RegExpPrototype.shape.Lookup(compileKey)
	assert.True(t, ok)
}

// TestRegExpCompileRealm: a RegExp a host hands to another realm is not
// that realm's to recompile.
func TestRegExpCompileRealm(t *testing.T) {
	for _, shared := range []bool{false, true} {
		a, b := NewRealmWith(RealmOptions{SharedIntrinsics: shared}), NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		rx, err := a.NewRegExp(FromGoString("a"), AtomEmpty)
		require.NoError(t, err)
		compile, err := b.RegExpPrototype.GetProp(b, compileKey)
		require.NoError(t, err)
		_, err = b.Call(compile, ObjectValue(rx), []Value{StringValue(FromGoString("b"))})
		assert.Equal(t, "TypeError: RegExp.prototype.compile requires a RegExp created by this realm's RegExp constructor", errorString(err))
		assert.Equal(t, "a", rx.RegExpData().Source().GoString())
		compile, err = a.RegExpPrototype.GetProp(a, compileKey)
		require.NoError(t, err)
		_, err = a.Call(compile, ObjectValue(rx), []Value{StringValue(FromGoString("b"))})
		require.NoError(t, err)
		assert.Equal(t, "b", rx.RegExpData().Source().GoString())
	}
}

// staticsLib reads the statics: st() lists input, lastMatch, lastParen,
// leftContext, rightContext and $1-$3; all() adds $4-$9. set(k, v) calls
// the setter of k, which an assignment does not reach in a shared realm.
const staticsLib = `
const st = () => [RegExp.input, RegExp.lastMatch, RegExp.lastParen, RegExp.leftContext, RegExp.rightContext,
	RegExp.$1, RegExp.$2, RegExp.$3].join("|");
const all = () => st() + "|" + [RegExp.$4, RegExp.$5, RegExp.$6, RegExp.$7, RegExp.$8, RegExp.$9].join("|");
const set = (k, v) => Object.getOwnPropertyDescriptor(RegExp, k).set.call(RegExp, v);
`

// TestRegExpStatics covers the legacy static accessors of %RegExp% (the
// legacy RegExp features proposal) in both kinds of realm: the match each
// builtin path records, the input setter, invalidation and the receiver
// checks.
func TestRegExpStatics(t *testing.T) {
	cases := []protoCase{
		{"initial", `return all()`, "|||||||||||||"},
		{"exec", `/(b)(c)?/.exec("abd"); return st()`, "abd|b||a|d|b||"},
		{"aliases", `/(b)/.exec("abc"); return [RegExp.$_, RegExp["$&"], RegExp["$+"], RegExp["$\x60"], RegExp["$'"]].join("|")`,
			"abc|b|b|a|c"},
		{"nine groups", `/(a)(b)(c)(d)(e)(f)(g)(h)(i)(j)/.exec("abcdefghij"); return all()`,
			"abcdefghij|abcdefghij|j|||a|b|c|d|e|f|g|h|i"},
		{"no groups", `/(x)/.exec("x"); /b/.exec("abc"); return st()`, "abc|b||a|c|||"},
		{"exec: global", `const re = /a(.)/g; re.exec("a1a2"); re.exec("a1a2"); return st()`, "a1a2|a2|2|a1||2||"},
		{"exec: sticky", `const re = /a(.)/y; re.lastIndex = 2; re.exec("a1a2"); return st()`, "a1a2|a2|2|a1||2||"},
		{"exec: failure keeps", `/(a)/.exec("xa"); /(b)/.exec("x"); /(c)/g.exec("c c"); return st()`, "c c|c|c|| c|c||"},
		{"exec: global failure keeps", `/(a)/.exec("xa"); const re = /(b)/g; re.lastIndex = 5; re.exec("b"); return st()`, "xa|a|a|x||a||"},
		{"exec: lastIndex not writable", `/(x)/.exec("x"); const re = /(a)/g; Object.defineProperty(re, "lastIndex", {value: 0, writable: false});
			try { re.exec("a"); } catch (e) { return e.name + ":" + st(); }`, "TypeError:x|x|x|||x||"},
		{"exec: lastIndex valueOf", `const re = /(a)/g; re.lastIndex = {valueOf() { /(z)/.exec("z"); return 1; }};
			re.exec("aa"); return st()`, "aa|a|a|a||a||"},
		{"test", `/(\d+)/.test("ab12cd"); return st()`, "ab12cd|12|12|ab|cd|12||"},
		{"test: simple class", `/\s/.test("a b"); return st()`, "a b| ||a|b|||"},
		{"test: backtracking", `/(?<=a)(b)/.test("ab"); return st()`, "ab|b|b|a||b||"},
		{"test: global", `const re = /(\d)/g; re.test("1x2"); re.test("1x2"); return st()`, "1x2|2|2|1x||2||"},
		{"test: failure keeps", `/(a)/.test("a"); /\s/.test("x"); /(?<=q)b/.test("b"); return st()`, "a|a|a|||a||"},
		{"match", `"xay".match(/(a)/); return st()`, "xay|a|a|x|y|a||"},
		{"match: global", `"a1b2c3".match(/[a-z](\d)/g); return st()`, "a1b2c3|c3|3|a1b2||3||"},
		{"match: global simple class", `"a b c".match(/\s/g); return st()`, "a b c| ||a b|c|||"},
		{"match: global empty", `"ab".match(/(?:)/g); return st()`, "ab|||ab||||"},
		{"match: global failure keeps", `/(x)/.exec("x"); "abc".match(/z/g); "abc".match(/\s/g); return st()`, "x|x|x|||x||"},
		{"replace", `"a1b2".replace(/(\d)/, "#"); return st()`, "a1b2|1|1|a|b2|1||"},
		{"replace: global", `"a1b2".replace(/(\d)/g, "#"); return st()`, "a1b2|2|2|a1b||2||"},
		{"replace: global template", `"x-y-z".replace(/-(\w)/g, "+$1"); return st()`, "x-y-z|-z|z|x-y||z||"},
		{"replace: simple class", `"a b c".replace(/\s/g, "_"); return st()`, "a b c| ||a b|c|||"},
		{"replace: simple class once", `"a b c".replace(/\s/, "_"); return st()`, "a b c| ||a|b c|||"},
		{"replace: replacer sees the last match", `return "a1b2".replace(/(\d)/g, () => RegExp.$1 + RegExp.leftContext.length)`,
			"a23b23"},
		{"replace: replacer once", `return "xay".replace(/(a)/, () => RegExp.leftContext + RegExp.$1)`, "xxay"},
		{"replace: failure keeps", `/(x)/.exec("x"); "abc".replace(/z/g, ""); "abc".replace(/\s/, ""); "abc".replace(/(z)/, "$1");
			return st()`, "x|x|x|||x||"},
		{"replaceAll", `"a1b2".replaceAll(/(\d)/g, "#"); return st()`, "a1b2|2|2|a1b||2||"},
		{"split", `"a,b;c".split(/([,;])/); return st()`, "a,b;c|;|;|a,b|c|;||"},
		{"split: empty matches", `"abc".split(/(?:)/); return st()`, "abc|||ab|c|||"},
		{"split: an empty match last", `"a,b".split(/,?/); return st()`, "a,b|||a,|b|||"},
		{"split: empty subject", `/(x)/.exec("x"); "".split(/a*/); return st()`, "|||||||"},
		{"split: empty subject, no match", `/(x)/.exec("x"); "".split(/a/); return st()`, "x|x|x|||x||"},
		{"split: limit", `"a,b,c".split(/(,)/, 2); return st()`, "a,b,c|,|,|a|b,c|,||"},
		{"split: limit 0", `/(x)/.exec("x"); "a,b".split(/,/, 0); return st()`, "x|x|x|||x||"},
		{"search", `const re = /(b)/g; re.lastIndex = 2; "abc".search(re); return st() + re.lastIndex`, "abc|b|b|a|c|b||2"},
		{"matchAll", `[..."a1b2".matchAll(/[a-z](\d)/g)]; return st()`, "a1b2|b2|2|a1||2||"},
		{"u: surrogates", `/(.)/u.exec("\u{1F600}x"); return [RegExp.$1.length, RegExp.rightContext].join()`, "2,x"},
		{"u: global", `"a\u{1F600}b".match(/./gu); return [RegExp.lastMatch, RegExp.leftContext.length].join()`, "b,3"},
		{"UTF-16 subject", `/(中+)文/.exec("x中中文y"); return st()`, "x中中文y|中中文|中中|x|y|中中||"},
		{"UTF-16 global", `"中1文2".replace(/(\d)/g, ""); return st()`, "中1文2|2|2|中1文||2||"},
		{"named groups", `/(?<x>a)(?<y>b)/.exec("ab"); return st()`, "ab|ab|b|||a|b|"},
		{"d flag", `const m = /(?<x>a)/d.exec("ba"); return [m.indices[1].join("-"), m.indices.groups.x.join("-"), Object.keys(m).join(),
			Object.keys(m.indices).join(), RegExp.$1].join("|")`, "1-2|1-2|0,1,index,input,groups,indices|0,1,groups|a"},
		{"read again after another match", `const re = /(\d)/g; re.exec("12"); const a = RegExp.$1; re.exec("12"); return a + RegExp.$1`, "12"},
		{"read again after the same match", `const re = /(\d)/; re.exec("12"); const a = RegExp.$1; re.exec("12"); return a + RegExp.$1`, "11"},
		{"recompiled", `const re = /(a)/; re.exec("ab"); re.compile("(b)"); return RegExp.$1`, "a"},

		// The input setter. An assignment to a shared realm's frozen %RegExp%
		// is rejected before any setter runs (DESIGN §8.1).
		{"assignment", `let v; try { RegExp.input = "x"; v = RegExp.input; } catch (e) { v = e.message; }
			return Object.isFrozen(RegExp) && v.startsWith("Cannot modify property 'input' of shared intrinsic") ? "x" : v`, "x"},
		{"input setter", `set("input", 5); return [RegExp.input, RegExp.$_, RegExp.lastMatch].join("|")`, "5|5|"},
		{"$_ setter", `set("$_", "a"); return RegExp.input`, "a"},
		{"setter keeps the match", `/(a)/.exec("xay"); set("input", "zz"); return st()`, "zz|a|a|x|y|a||"},
		{"setter keeps a read match", `/(a)/.exec("xay"); RegExp.$1; set("input", "zz"); set("$_", "q"); return st()`, "q|a|a|x|y|a||"},
		{"setter then a match", `set("input", "zz"); /(b)/.exec("b"); return st()`, "b|b|b|||b||"},
		{"setter then the same match", `/(a)/.exec("a"); RegExp.$1; set("input", "zz"); /(a)/.exec("a"); return st()`, "a|a|a|||a||"},
		{"setter converts first", `/(a)/.exec("a"); set("input", {toString() { /(q)/.exec("xq"); return "s"; }}); return st()`,
			"s|q|q|x||q||"},
		{"setter conversion throws", `/(a)/.exec("a"); try { set("input", {toString() { throw new Error("t"); }}); } catch (e) { return e.message + st(); }`,
			"ta|a|a|||a||"},

		// Invalidation.
		{"subclass", `class R extends RegExp {} new R("(a)").exec("a");
			const out = []; for (const k of ["input", "$_", "lastMatch", "$1", "$9", "leftContext"]) { try { RegExp[k]; out.push(k); } catch (e) { out.push(e.name); } }
			return out.join()`, "TypeError,TypeError,TypeError,TypeError,TypeError,TypeError"},
		{"subclass: replace", `class R extends RegExp {} "a".replace(new R("a", "g"), ""); return RegExp.$1`, "!TypeError"},
		{"subclass: failure keeps", `/(a)/.exec("a"); class R extends RegExp {} new R("b").exec("a"); return RegExp.$1`, "a"},
		{"other newTarget", `function F() {} F.prototype = RegExp.prototype; const re = Reflect.construct(RegExp, ["(a)"], F);
			/(x)/.exec("x"); re.test("a"); return RegExp.$1`, "!TypeError"},
		{"other newTarget: simple class", `function F() {} F.prototype = RegExp.prototype; const re = Reflect.construct(RegExp, ["\\s"], F);
			/(x)/.exec("x"); re.test(" "); return RegExp.lastMatch`, "!TypeError"},
		{"other newTarget: global", `function F() {} F.prototype = RegExp.prototype; const re = Reflect.construct(RegExp, ["a", "g"], F);
			/(x)/.exec("x"); "aa".replace(re, ""); return RegExp.lastMatch`, "!TypeError"},
		{"split: a subclass's splitter", `class R extends RegExp {} "a,b".split(new R(",")); return RegExp.lastMatch`, "!TypeError"},
		{"match restores", `class R extends RegExp {} new R("(a)").exec("a"); /(b)/.exec("b"); return st()`, "b|b|b|||b||"},
		{"setter after invalidation", `class R extends RegExp {} new R("(a)").exec("a"); set("input", "x");
			try { RegExp.$1; } catch (e) { return RegExp.input + e.name; }`, "xTypeError"},
		{"setter twice after invalidation", `class R extends RegExp {} new R("(a)").exec("a"); set("input", "x"); set("input", "y");
			try { RegExp.lastMatch; } catch (e) { return RegExp.input + e.name; }`, "yTypeError"},

		// The receiver.
		{"getter: other receiver", `Object.getOwnPropertyDescriptor(RegExp, "$1").get.call({})`, "!TypeError"},
		{"getter: subclass", `class R extends RegExp {} return R.$1`, "!TypeError"},
		{"getter: inheriting object", `return Object.create(RegExp).lastMatch`, "!TypeError"},
		{"getter: undefined", `Object.getOwnPropertyDescriptor(RegExp, "input").get()`, "!TypeError"},
		{"setter: other receiver", `Object.getOwnPropertyDescriptor(RegExp, "input").set.call({}, "x")`, "!TypeError"},
		{"setter: receiver before conversion", `Object.getOwnPropertyDescriptor(RegExp, "$_").set.call(RegExp.prototype, {toString() { throw new Error("t"); }})`,
			"!TypeError"},
		{"setter: subclass", `class R extends RegExp {} R.input = "x"; return RegExp.input + "|" + Object.hasOwn(R, "input")`, "!TypeError"},
		{"no setter", `"use strict"; /(a)/.exec("a"); RegExp.$1 = "b"`, "!TypeError"},

		// The properties.
		// A shared realm's intrinsics are frozen.
		{"descriptors", `const out = [];
			for (const k of ["input", "$_", "lastMatch", "$&", "lastParen", "$+", "leftContext", "$\x60", "rightContext", "$'", "$1", "$5", "$9"]) {
				const d = Object.getOwnPropertyDescriptor(RegExp, k);
				out.push([k, d.enumerable, d.configurable !== Object.isFrozen(RegExp), d.get.name, d.get.length, d.set && d.set.name, d.set && d.set.length].join(":"));
			}
			return out.join(" ")`,
			"input:false:true:get input:0:set input:1 $_:false:true:get $_:0:set $_:1 lastMatch:false:true:get lastMatch:0:: " +
				"$&:false:true:get $&:0:: lastParen:false:true:get lastParen:0:: $+:false:true:get $+:0:: " +
				"leftContext:false:true:get leftContext:0:: $`:false:true:get $`:0:: rightContext:false:true:get rightContext:0:: " +
				"$':false:true:get $':0:: $1:false:true:get $1:0:: $5:false:true:get $5:0:: $9:false:true:get $9:0::"},
		{"function sources", `const src = k => { const d = Object.getOwnPropertyDescriptor(RegExp, k); return d.get + (d.set ? "|" + d.set : ""); };
			return ["input", "$_", "lastMatch", "$&", "$+", "$\x60", "$'", "$1"].map(src).join("|")`,
			"function get input() { [native code] }|function set input() { [native code] }|" +
				"function get $_() { [native code] }|function set $_() { [native code] }|function get lastMatch() { [native code] }|" +
				`function get ["$&"]() { [native code] }|function get ["$+"]() { [native code] }|` +
				"function get [\"$`\"]() { [native code] }|" + `function get ["$'"]() { [native code] }|function get $1() { [native code] }`},
		{"distinct functions", `const g = k => Object.getOwnPropertyDescriptor(RegExp, k);
			return [g("input").get === g("$_").get, g("input").set === g("$_").set, g("$1").get === g("$2").get].join()`, "false,false,false"},
		{"not enumerable", `return Object.keys(RegExp).join() + "|" + JSON.stringify(Object.assign({}, RegExp))`, "|{}"},
		{"subclass inherits the accessors", `class R extends RegExp {} return "$1" in R && !Object.hasOwn(R, "$1")`, "true"},
	}
	for i := range cases {
		cases[i].body = staticsLib + cases[i].body
	}
	runProtoCases(t, cases)
}

// TestRegExpStaticsMutable covers the pending statics of a mutable realm's
// %RegExp%: every way to observe the constructor defines them where the
// shared template has them, and the realm's matches are recorded before.
func TestRegExpStaticsMutable(t *testing.T) {
	runMutableProtoCases(t, []protoCase{
		{"recorded before", `/(a)/.exec("xa"); return RegExp.leftContext + RegExp.$1`, "xa"},
		{"write first", `RegExp.foo = 1; return Object.getOwnPropertyNames(RegExp).slice(-2).join()`, "$9,foo"},
		{"define first", `Object.defineProperty(RegExp, "foo", {value: 1}); return Object.getOwnPropertyNames(RegExp).slice(-2).join()`, "$9,foo"},
		{"delete first", `delete RegExp.name; return [typeof RegExp.$1, "name" in Object.getOwnPropertyNames(RegExp)].join()`, "string,false"},
		{"delete one", `delete RegExp.lastMatch; return ["lastMatch" in RegExp, "$&" in RegExp].join()`, "false,true"},
		{"delete after a match", `/(a)/.exec("a"); const d = delete RegExp.$1; return [d, "$1" in RegExp, typeof RegExp.$1, RegExp.lastMatch].join()`,
			"true,false,undefined,a"},
		{"delete input, then match", `delete RegExp.input; /(a)/.exec("xa"); return [RegExp.input, RegExp.$1, RegExp.leftContext].join()`, ",a,x"},
		{"replace one", `Object.defineProperty(RegExp, "$1", {value: 1}); /(a)/.exec("a"); return RegExp.$1`, "1"},
		{"assign one", `RegExp.input = "x"; return RegExp.input`, "x"},
		{"has", `return ["$1" in RegExp, "$_" in RegExp, "$0" in RegExp, "$10" in RegExp].join()`, "true,true,false,false"},
		{"hasOwn", `return Object.hasOwn(RegExp, "rightContext")`, "true"},
		{"own descriptor", `return Object.getOwnPropertyDescriptor(RegExp, "$+").get.name`, "get $+"},
		{"shadows Function.prototype", `Function.prototype.$1 = "p"; return typeof RegExp.$1`, "string"},
		{"cache filled on a constructor of the same shape", `Function.prototype.$1 = "p"; const get = o => o.$1;
			get(Set); get(Set); return [get(Set), get(RegExp), get(Set)].join("|")`, "p||p"},
		{"cache filled on Set, other key", `const get = o => o.length; get(Set); get(Set); return [get(Set), get(RegExp)].join()`, "0,2"},
		{"other keys pending", `return [RegExp.name, RegExp.length, typeof RegExp.prototype.exec, RegExp[Symbol.species] === RegExp].join()`,
			"RegExp,2,function,true"},
		{"prevent extensions", `Object.preventExtensions(RegExp); return typeof RegExp.$1`, "string"},
		{"freeze", `Object.freeze(RegExp); /(a)/.exec("a"); return Object.isFrozen(RegExp) && RegExp.$1`, "a"},
		{"new prototype", `Object.setPrototypeOf(RegExp, null); return typeof RegExp.$1`, "string"},
		{"species replaced", `Object.defineProperty(RegExp, Symbol.species, {value: undefined}); return typeof RegExp.$1 + "a,b".split(/,/).length`,
			"string2"},
		{"instanceof", `return [/a/ instanceof RegExp, {} instanceof RegExp, typeof RegExp.$1].join()`, "true,false,string"},
		{"pristine paths", `const s = "a-b".replace(/-/, "+") + "a-b".split(/-/).join("+") + /b/.test("b");
			RegExp.$1; return s + "a-b".replace(/-/, "+") + "a-b".split(/-/).join("+") + /b/.test("b") + RegExp.lastMatch`,
			"a+ba+btruea+ba+btrueb"},
	})
}

// TestRegExpStaticsKeyOrder: a mutable realm lists %RegExp%'s keys in the
// shared template's order, however the pending statics are defined.
func TestRegExpStaticsKeyOrder(t *testing.T) {
	keys := `return Reflect.ownKeys(RegExp).map(String).join()`
	want := evalProtoBody(t, keys, true)
	assert.Equal(t, "length,name,prototype,input,$_,lastMatch,$&,lastParen,$+,leftContext,$`,rightContext,$',"+
		"$1,$2,$3,$4,$5,$6,$7,$8,$9,Symbol(Symbol.species)", want)
	assert.Equal(t, want, evalProtoBody(t, keys, false), "listed first")
	assert.Equal(t, want, evalProtoBody(t, `RegExp.$1; `+keys, false), "looked up first")
	assert.Equal(t, want, evalProtoBody(t, `/a/.exec("a"); "a".replace(/a/, "b"); RegExp.name; `+keys, false), "after the pristine paths")
}

// TestRegExpStaticsLazy: a mutable realm defines the statics on first use
// only, recording matches and keeping the guards meanwhile; a shared realm
// has them from the template.
func TestRegExpStaticsLazy(t *testing.T) {
	r := NewRealm()
	c := r.RegExpCtor
	require.True(t, c.flags&flagHasLazy != 0)
	require.IsType(t, (*FunctionData)(nil), c.internal)
	assert.Equal(t, noFillStatics, c.shape.noFill, "only a static's fill is refused")
	assert.Same(t, r.SetCtor.shape, c.shape, "the shape Set shares")
	key := StringKey(staticAtom("$1"))
	_, _, ok := c.shape.Lookup(key)
	assert.False(t, ok)
	assert.True(t, r.lacksWellKnown(c, SymbolKey(SymToPrimitive)))
	m, err := r.hasInstanceMethod(c)
	require.NoError(t, err)
	assert.Nil(t, m, "instanceof keeps OrdinaryHasInstance")
	assert.True(t, r.guardHolds(&r.protoGuards[guardRegExpSpecies]))
	assert.NotNil(t, r.protoGuards[guardRegExpSpecies].shape, "the species guard caches")

	rx, err := r.NewRegExp(FromGoString("(b)"), AtomEmpty)
	require.NoError(t, err)
	exec, err := rx.GetProp(r, StringKey(staticAtom("exec")))
	require.NoError(t, err)
	_, err = r.Call(exec, ObjectValue(rx), []Value{StringValue(FromGoString("abc"))})
	require.NoError(t, err)
	assert.NotZero(t, c.flags&flagHasLazy, "a match defines nothing")
	assert.Same(t, r.regexps.lastC, rx.RegExpData().c)
	assert.Nil(t, r.lazy, "nor reads the match again")

	v, err := c.GetProp(r, key)
	require.NoError(t, err)
	assert.Equal(t, "b", v.String())
	assert.Zero(t, c.flags&flagHasLazy)
	_, _, ok = c.shape.Lookup(key)
	assert.True(t, ok)
	assert.True(t, r.guardHolds(&r.protoGuards[guardRegExpSpecies]))
	m, err = r.hasInstanceMethod(c)
	require.NoError(t, err)
	assert.Nil(t, m)

	s := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	assert.Zero(t, s.RegExpCtor.flags&flagHasLazy)
	_, _, ok = s.RegExpCtor.shape.Lookup(key)
	assert.True(t, ok)
}

// TestRegExpStaticsSharedShapeIC: in a mutable realm, the Set and
// SharedArrayBuffer constructors share %RegExp%'s shape while the statics
// are pending. Reads of their properties fill inline caches before and
// after the statics are defined. Only a static's entry on that shape is
// refused, as it would hit %RegExp% with the statics pending.
func TestRegExpStaticsSharedShapeIC(t *testing.T) {
	r := NewRealm()
	eval := func(src string) Value {
		s, err := syntax.ParseScript("ic.js", src, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileScript(s)
		require.NoError(t, err)
		v, err := r.RunScript(code)
		require.NoError(t, err)
		return v
	}
	v := eval(`Function.prototype.$1 = "p";
[o => o.length, o => o.prototype, o => o.$1, Set, SharedArrayBuffer]`)
	elem := func(i int) *Object {
		e, err := v.AsObject().Get(r, IndexKey(uint32(i)), v)
		require.NoError(t, err)
		return e.AsObject()
	}
	c := r.RegExpCtor
	ctors := []*Object{elem(3), elem(4)}
	for _, o := range ctors {
		require.Same(t, c.shape, o.shape, "the shape %RegExp% shares")
	}
	// fill reads the key of site i on o with the site's entry cleared.
	fill := func(i int, o *Object) *Shape {
		f := elem(i)
		fd := f.internal.(*FunctionData)
		if fd.icBase != icUnbound {
			r.ic[fd.icBase] = ICEntry{}
		}
		_, err := r.Call(ObjectValue(f), Undefined(), []Value{ObjectValue(o)})
		require.NoError(t, err)
		return r.ic[fd.icBase].Shape
	}
	for _, pending := range []bool{true, false} {
		require.Equal(t, pending, c.flags&flagHasLazy != 0)
		for _, o := range ctors {
			assert.Same(t, o.shape, fill(0, o), "length, pending=%v", pending)
			assert.Same(t, o.shape, fill(1, o), "prototype, pending=%v", pending)
			if pending {
				assert.Nil(t, fill(2, o), "a static on the shape %RegExp% shares")
			} else {
				assert.Same(t, o.shape, fill(2, o), "a static, once defined")
			}
		}
		eval(`RegExp.$1`)
	}
	assert.NotSame(t, c.shape, ctors[0].shape)
}

// TestRegExpStaticsRealm: the statics are per realm, and a match by
// another realm's RegExp invalidates them (where the proposal ignores
// it). A mutable realm's accessors refuse another mutable realm's
// %RegExp%; realms with shared intrinsics share it, and each reads its own
// statics.
func TestRegExpStaticsRealm(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			a, b := NewRealmWith(RealmOptions{SharedIntrinsics: shared}), NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			get := func(r *Realm, recv *Object, name string) string {
				t.Helper()
				v, err := r.RegExpCtor.Get(r, StringKey(staticAtom(name)), ObjectValue(recv))
				if err != nil {
					return errorString(err)
				}
				return v.String()
			}
			exec := func(r *Realm, rx *Object, s string) {
				t.Helper()
				f, err := r.RegExpPrototype.GetProp(r, StringKey(staticAtom("exec")))
				require.NoError(t, err)
				_, err = r.Call(f, ObjectValue(rx), []Value{StringValue(FromGoString(s))})
				require.NoError(t, err)
			}
			ra, err := a.NewRegExp(FromGoString("(a)"), AtomEmpty)
			require.NoError(t, err)
			rb, err := b.NewRegExp(FromGoString("(b)"), AtomEmpty)
			require.NoError(t, err)
			exec(a, ra, "xa")
			exec(b, rb, "b")
			assert.Equal(t, "a", get(a, a.RegExpCtor, "$1"))
			assert.Equal(t, "b", get(b, b.RegExpCtor, "$1"))

			exec(b, ra, "a")
			assert.Equal(t, "TypeError: RegExp legacy static properties are unavailable after a match by a RegExp subclass instance or another realm's RegExp",
				get(b, b.RegExpCtor, "$1"))
			assert.Equal(t, "a", get(a, a.RegExpCtor, "$1"), "the other realm's statics stay")
			assert.Equal(t, "x", get(a, a.RegExpCtor, "leftContext"))

			if shared {
				assert.Same(t, a.RegExpCtor, b.RegExpCtor)
			} else {
				assert.Equal(t, "TypeError: RegExp legacy static getter called on function RegExp() { [native code] }, not the RegExp constructor",
					get(b, a.RegExpCtor, "$1"))
			}
		})
	}
}

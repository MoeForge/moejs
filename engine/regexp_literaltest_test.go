package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRegExpLiteralTest checks /lit/.test(x) of a local x, which matches
// without the literal's object while RegExp.prototype.test and exec are
// the originals (RegExpTest): the results, the statics and the errors
// are those of the call, and a patched test or exec, a global or sticky
// literal and lastIndex reads take the call. The expectations are
// Node 24's.
func TestRegExpLiteralTest(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"match", []string{`function f(s) { return /^on[A-Z]/.test(s); } [f("onClick"), f("one"), f("on"), f("")].join()`}, "true,false,false,false"},
		{"in a condition", []string{`function f(h) { if (h === "" || /^0+$/.test(h)) return 1; return 2; } [f(""), f("000"), f("010")].join()`}, "1,1,2"},
		{"flags", []string{`function f(s) { return [/^ON/i.test(s), /^a.b$/s.test(s), /^b$/m.test(s), /^\u{1F600}$/u.test(s), /^[\p{L}--[a-z]]$/v.test(s)].join(); } [f("on"), f("a\nb"), f("\u{1F600}"), f("é")].join("|")`}, "true,false,false,false,false|false,true,true,false,false|false,false,false,true,false|false,false,false,false,true"},
		{"wide subject", []string{`function f(s) { return /é+$/.test(s); } [f("café"), f("cafe"), f("中é")].join()`}, "true,false,true"},
		{"backtracking", []string{`function f(s) { return /^(a+)\1b$/.test(s); } [f("aaaab"), f("aaab")].join()`}, "true,false"},
		{"converted argument", []string{`function f(s) { return /^1$/.test(s); } var o = { toString() { return "1"; } }; [f(1), f(o), f(undefined), f(null)].join()`}, "true,true,false,false"},
		{"argument that throws", []string{`function f(s) { return /a/.test(s); } var r; try { f(Symbol()); } catch (e) { r = e instanceof TypeError; } r`}, "true"},
		{"statics", []string{`function f(s) { return /(b)c/.test(s); } f("abcd"); var a = [RegExp.lastMatch, RegExp.$1, RegExp.leftContext, RegExp.rightContext, RegExp.input].join(); f("zzz"); a + "|" + RegExp.lastMatch`}, "bc,b,a,d,abcd|bc"},
		{"patched exec", []string{`var n = 0, orig = RegExp.prototype.exec; RegExp.prototype.exec = function (s) { n++; return orig.call(this, s); }; function f(s) { return /b/.test(s); } var r = [f("abc"), f("x")].join(); RegExp.prototype.exec = orig; [r, n, f("b"), n].join()`}, "true,false,2,true,2"},
		{"exec sees a fresh literal", []string{`var seen = [], orig = RegExp.prototype.exec; RegExp.prototype.exec = function (s) { seen.push(this); return orig.call(this, s); }; function f(s) { return /a/.test(s); } f("a"); f("a"); RegExp.prototype.exec = orig; [seen.length, seen[0] !== seen[1], seen[0].lastIndex, seen[0].source].join()`}, "2,true,0,a"},
		{"exec getter", []string{`var d = Object.getOwnPropertyDescriptor(RegExp.prototype, "exec"); Object.defineProperty(RegExp.prototype, "exec", { get() { return () => null; }, configurable: true }); function f(s) { return /a/.test(s); } var r = f("a"); Object.defineProperty(RegExp.prototype, "exec", d); r + "," + f("a")`}, "false,true"},
		{"patched test", []string{`var orig = RegExp.prototype.test; RegExp.prototype.test = function (s) { return "patched:" + s + ":" + this.source; }; function f(s) { return /a/.test(s); } var r = f("a"); RegExp.prototype.test = orig; r + "," + f("a")`}, "patched:a:a,true"},
		{"test getter reads late", []string{`var x = "a", orig = RegExp.prototype.test; Object.defineProperty(RegExp.prototype, "test", { get() { x = "b"; return orig; }, configurable: true }); function f() { return /a/.test(x); } f()`}, "false"},
		{"deleted test", []string{`delete RegExp.prototype.test; Object.prototype.test = function () { return "object"; }; function f(s) { return /a/.test(s); } f("a")`}, "object"},
		{"patched flags and symbols", []string{`Object.defineProperty(RegExp.prototype, "global", { get() { return true; } }); RegExp.prototype[Symbol.match] = () => "m"; Object.defineProperty(RegExp.prototype, "flags", { get() { return "g"; } }); function f(s) { return /a/.test(s); } [f("a"), f("b"), "a".match(/a/)].join()`}, "true,false,m"},
		{"global literal", []string{`function f(s) { return /a/g.test(s); } [f("aa"), f("aa"), f("b")].join()`}, "true,true,false"},
		{"sticky literal", []string{`function f(s) { return /a/y.test(s); } [f("ba"), f("ab")].join()`}, "false,true"},
		{"lastIndex set by the program", []string{`function f(s) { var re = /a/g; re.lastIndex = 1; return re.test(s) + ":" + re.lastIndex; } function g(s) { var re = /a/; re.lastIndex = 5; return re.test(s) + ":" + re.lastIndex; } [f("aa"), f("ab"), g("a")].join()`}, "true:2,false:0,true:5"},
		{"prototype lastIndex", []string{`Object.defineProperty(RegExp.prototype, "lastIndex", { get() { throw new Error("read"); }, set(v) { throw new Error("write"); } }); function f(s) { return /a/.test(s); } f("a")`}, "true"},
		{"captured argument", []string{`function f(s) { var g = () => s; return /a/.test(s) + g(); } f("a")`}, "truea"},
		{"strict", []string{`"use strict"; function f(s) { return /a/.test(s); } [f("a"), f("b")].join()`}, "true,false"},
	})
}

// TestRegExpLiteralTestShared runs /lit/.test(x) in a shared realm, whose
// intrinsics no code can patch.
func TestRegExpLiteralTestShared(t *testing.T) {
	assert.Equal(t, "true,false", evalProtoBody(t, `const f = s => /^on[A-Z]/.test(s); return [f("onA"), f("on")].join()`, true))
}

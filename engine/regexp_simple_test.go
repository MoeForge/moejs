package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSimpleClassRecognizer pins which patterns take the single-class path.
func TestSimpleClassRecognizer(t *testing.T) {
	accepted := []string{`a`, `&`, `"`, `-`, `a+`, `<{2}`, `\/`, `\.`, `\$+`, `\]`, `é`, `\s+`, `\d`, `\S`, `\w{2,4}`, `[a-z]+`, `[^a-z0-9]+`, `[\s,]+`, `[\t\n ]`, `[-a]`, `[a-]`, `[\d-x]`, `[\x41-\x5a]{3}`, `[ ]+`, `[]`, `[^]`, `[\]\-\\]+`, `\d{2,}`, `[\b]`, `[\0]`, `[\s--1]`, `[\d-\s-a]`, `[\uD83D]`, `[\ud800-\udfff]+`, `[\x00-\x07\x0b\x0e-\x1f<\u2028\u2029\ud800-\udfff]`}
	for _, p := range accepted {
		assert.NotNil(t, compileSimpleClass(FromGoString(p).UTF16(), regexpFlags{global: true}), p)
	}
	rejected := []string{``, `ab`, `a*`, `]`, `{`, `}`, `|`, `$`, `^`, `\-`, `\uD83D`, `.`, `\s*`, `\s?`, `\s+?`, `\d{0,3}`, `\b`, `\s+\d`, `(\s)`, `[a-\d]`, `[z-a]`, `\p{L}`, `[\q]`, `\01`, `\0`, `\d{1001}`, `[a-z`, `\s+x`}
	for _, p := range rejected {
		assert.Nil(t, compileSimpleClass(FromGoString(p).UTF16(), regexpFlags{global: true}), p)
	}
	assert.Nil(t, compileSimpleClass(FromGoString(`\s+`).UTF16(), regexpFlags{ignoreCase: true}), "i flag")
	assert.Nil(t, compileSimpleClass(FromGoString(`[^a]+`).UTF16(), regexpFlags{unicode: true}), "negated class in u mode")
	assert.Nil(t, compileSimpleClass(FromGoString(`\S`).UTF16(), regexpFlags{unicode: true}), "complement escape in u mode")
	assert.NotNil(t, compileSimpleClass(FromGoString(`\s+`).UTF16(), regexpFlags{unicode: true}))
	// In u mode a surrogate is half of a code point: escaped, literal or in
	// a range, it leaves the class to the translator.
	for _, p := range []string{`[\uD83D]`, `[\ud800-\udfff]+`, `[\x00-\x1f\ud800-\udfff]`} {
		assert.Nil(t, compileSimpleClass(FromGoString(p).UTF16(), regexpFlags{unicode: true}), "u mode %q", p)
	}
	// Literal surrogate units: one unit each without u.
	for _, p := range [][]uint16{{0xD83D}, {'[', 0xD83D, ']'}, {'[', 'a', 0xDC00, '-', 0xDFFF, ']', '+'}} {
		assert.NotNil(t, compileSimpleClass(p, regexpFlags{}), "%x", p)
		assert.Nil(t, compileSimpleClass(p, regexpFlags{unicode: true}), "u mode %x", p)
	}
}

// TestSimpleClassMatchesTranslator runs replace, test and match with the
// single-class matcher and with the translated RE2 program on the same
// patterns and subjects (ASCII, UTF-16, surrogates) and requires identical
// results.
func TestSimpleClassMatchesTranslator(t *testing.T) {
	r := NewRealm()
	patterns := []struct{ src, flags string }{
		{`\s+`, "g"}, {`\s+`, ""}, {`\s+`, "gu"}, {`\s`, "g"}, {`\S+`, "g"}, {`\d+`, "g"}, {`\d{2,3}`, "g"}, {`\d{2}`, "g"},
		{`\d{2,}`, "g"}, {`\w+`, "g"}, {`\W+`, "g"}, {`[a-z]+`, "g"}, {`[^a-z]+`, "g"}, {`[^\s]+`, "g"}, {`[\s,;]+`, "g"},
		{`[\x00-\x1f]+`, "g"}, {`[ 　]+`, "g"}, {`[^]`, "g"}, {`[]`, "g"}, {`[éü]+`, "g"}, {`[Ѐ-ӿ]+`, "gu"},
		{`[-.]+`, "g"}, {`[.\-]`, "g"}, {`\s+`, "gm"}, {`\d+`, "gs"},
		{`[\s--1]`, "g"}, {`[\d-x]+`, "g"}, {`[\d-\s-a]+`, "g"}, {`[\w-.]+`, "g"},
		{`&`, "g"}, {`"`, "g"}, {`e`, ""}, {`\.`, "g"}, {`-+`, "g"}, {`ü`, "gu"}, {`\/`, "g"},
		{`["'&<>]`, ""}, {`["'&<>]`, "g"}, {`["'&<>]`, "d"}, {`[\ud800-\udfff]`, ""}, {`[\ud800-\udfff]+`, "g"},
		{`[\x00-\x07\x0b\x0e-\x1f<\u2028\u2029\ud800-\udfff]`, ""}, {`[\x00-\x07\x0b\x0e-\x1f<\u2028\u2029\ud800-\udfff]`, "g"},
		{`[\uDE00a]`, "g"}, {`[^\ud83d]+`, "g"}, {`\s+`, "y"}, {`[a-z]`, "gy"},
	}
	var subjects []*String
	for _, g := range []string{
		"", " ", "a", "hello   world\t\n\r\v\f  end", "  leading and trailing  ", "12345 67 8 9012 x1", "a1b22c333d4444",
		"tabs\tand nbsp　ideographic sep", "héllo wörld  über  straße", "Привет  мир 42", "emoji 😀  pair 😀😀 end",
		"no-match-here", "--dots...and-dashes--", "\x00\x01 ctrl \x1f", `<a href="x">'&'</a>`, "line\u2028sep\u2029<",
	} {
		subjects = append(subjects, FromGoString(g))
	}
	lone := append(FromGoString("lone ").UTF16(), 0xD83D)
	lone = append(lone, FromGoString(" surrogate ").UTF16()...)
	lone = append(lone, 0xDC00)
	subjects = append(subjects, FromUTF16(append(lone, FromGoString(" here").UTF16()...)))
	templates := []string{" ", "", "$&", "[$&]", "$`", "$'", "$$", "$1", "x$"}
	for _, p := range patterns {
		rx, err := r.NewRegExp(FromGoString(p.src), FromGoString(p.flags))
		require.NoError(t, err, p.src)
		d := rx.RegExpData()
		require.NotNil(t, d.c.simple, "%s/%s should be recognized", p.src, p.flags)
		simple := d.c.simple
		run := func(fast bool, f func() (Value, error)) any {
			if fast {
				d.c.simple = simple
			} else {
				d.c.simple = nil
			}
			if err := r.setRegExpLastIndex(rx, 0); err != nil {
				t.Fatal(err)
			}
			v, err := f()
			require.NoError(t, err)
			return r.ToGo(v)
		}
		for _, subject := range subjects {
			s := StringValue(subject)
			subj := subject.GoString()
			for _, tmpl := range templates {
				f := func() (Value, error) { return regexpReplace(r, rx, d, s.AsString(), StringValue(FromGoString(tmpl))) }
				assert.Equal(t, run(false, f), run(true, f), "replace /%s/%s on %q with %q", p.src, p.flags, subj, tmpl)
			}
			fn, _ := r.FromGo(NativeFunc(func(_ *Realm, _ Value, args []Value) (Value, error) {
				m, _ := r.ToString(Arg(args, 0))
				pos, _ := r.ToString(Arg(args, 1))
				return StringValue(concat(concat(asciiString("<"), m), concat(asciiString("@"), pos))), nil
			}))
			f := func() (Value, error) { return regexpReplace(r, rx, d, s.AsString(), fn) }
			assert.Equal(t, run(false, f), run(true, f), "functional replace /%s/%s on %q", p.src, p.flags, subj)
			f = func() (Value, error) { return regexpMatch(r, rx, d, s.AsString()) }
			assert.Equal(t, run(false, f), run(true, f), "match /%s/%s on %q", p.src, p.flags, subj)
			if p.flags == "" {
				f = func() (Value, error) { return regexpProtoTest(r, ObjectValue(rx), []Value{s}) }
				assert.Equal(t, run(false, f), run(true, f), "test /%s/ on %q", p.src, subj)
			}
			// exec from every lastIndex, and test, which takes exec for g
			// and y: the result array, lastIndex after it and the statics.
			f = func() (Value, error) {
				var out []Value
				for last := 0; last <= subject.Len()+1; last++ {
					if err := r.setRegExpLastIndex(rx, last); err != nil {
						return Undefined(), err
					}
					res, err := r.RegExpExec(rx, s.AsString())
					if err != nil {
						return Undefined(), err
					}
					li, _ := rx.GetProp(r, lastIndexKey)
					out = append(out, li)
					if res.IsNull() {
						out = append(out, res)
						continue
					}
					o := res.AsObject()
					for _, k := range []string{"0", "length", "index", "input", "groups", "indices"} {
						v, err := o.GetProp(r, r.KeyFromGoString(k))
						if err != nil {
							return Undefined(), err
						}
						if v.IsObject() { // indices
							js, err := r.JSONStringify(v)
							if err != nil {
								return Undefined(), err
							}
							v = StringValue(js)
						}
						out = append(out, v)
					}
					for _, k := range []string{"lastMatch", "leftContext", "rightContext", "$_"} {
						v, err := r.RegExpCtor.GetProp(r, r.KeyFromGoString(k))
						if err != nil {
							return Undefined(), err
						}
						out = append(out, v)
					}
					if err := r.setRegExpLastIndex(rx, last); err != nil {
						return Undefined(), err
					}
					ok, err := regexpProtoTest(r, ObjectValue(rx), []Value{s})
					if err != nil {
						return Undefined(), err
					}
					li, _ = rx.GetProp(r, lastIndexKey)
					out = append(out, ok, li)
				}
				return ObjectValue(r.NewArrayFromSlice(out)), nil
			}
			assert.Equal(t, run(false, f), run(true, f), "exec /%s/%s on %q", p.src, p.flags, subj)
		}
		d.c.simple = simple
	}
}

// TestSimpleReplaceAllocations guards the hot path: a global whitespace
// collapse on a UTF-16 subject allocates the result and nothing else.
func TestSimpleReplaceAllocations(t *testing.T) {
	r := NewRealm()
	rx, err := r.NewRegExp(FromGoString(`\s+`), FromGoString("g"))
	require.NoError(t, err)
	s := FromGoString("Привет   мир,\n\tкак  дела? 请根据   用户描述  生成图片。")
	space := asciiString(" ")
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := regexpReplace(r, rx, rx.RegExpData(), s, StringValue(space)); err != nil {
			t.Fatal(err)
		}
	})
	assert.LessOrEqual(t, allocs, 2.0) // the UTF-16 buffer and the String header
}

// simpleExecCases are exec, test and match cases whose patterns, but for
// the i, y and u ones, take the single-class matcher; simpleExecWant is
// what Node.js 24 prints for them.
const simpleExecCases = `const E = "\ud83d\ude00", H = "\ud800";
const show = m => m === null ? "null" : JSON.stringify([...m]) + "@" + m.index + (m.indices ? JSON.stringify(m.indices) : "") + (m.groups === undefined ? "" : "G");
const cases = {
  escape: () => { const rx = /["'&<>]/; return show(rx.exec('a<b>')) + "," + rx.lastIndex; },
  global: () => { const rx = /["'&<>]/g, s = 'x<"y\''; const out = []; let m; while ((m = rx.exec(s))) out.push(show(m) + ":" + rx.lastIndex); out.push(rx.lastIndex); return out.join(" "); },
  past: () => { const rx = /a/g; rx.lastIndex = 9; return show(rx.exec("aa")) + "," + rx.lastIndex; },
  sticky: () => { const rx = /a/y; rx.lastIndex = 1; const a = show(rx.exec("ba")) + ":" + rx.lastIndex; rx.lastIndex = 1; return a + " " + show(rx.exec("bba")) + ":" + rx.lastIndex; },
  stickyGlobal: () => { const rx = /[a-c]+/gy; const out = []; for (const s of ["abcx", "xabc"]) { rx.lastIndex = 0; out.push(show(rx.exec(s)) + ":" + rx.lastIndex); } return out.join(" "); },
  ignoreCase: () => show(/[a-c]+/i.exec("xABCx")),
  indices: () => show(/[<>]+/d.exec("a<>b")),
  surrogateUnits: () => { const rx = /[\ud800-\udfff]/g, s = "a" + E + "b" + H; const out = []; let m; while ((m = rx.exec(s))) out.push(m.index + ":" + m[0].charCodeAt(0).toString(16) + ":" + rx.lastIndex); return out.join(" "); },
  surrogateU: () => { const rx = /[\ud800-\udfff]/gu, s = "a" + E + "b" + H; const out = []; let m; while ((m = rx.exec(s))) out.push(m.index + ":" + m[0].charCodeAt(0).toString(16) + ":" + rx.lastIndex); return out.join(" "); },
  seroval: () => { const rx = /[\x00-\x07\x0b\x0e-\x1f<\u2028\u2029\ud800-\udfff]/; return ["abc", "a<b", "\u2028", E, "\udc00", "\x0b", "\x0c"].map(s => rx.test(s)).join(); },
  insidePairU: () => { const rx = /[a-z]/gu; rx.lastIndex = 2; const s = "x" + E + "y"; const m = rx.exec(s); return show(m) + ":" + rx.lastIndex; },
  statics: () => { /["'&<>]/.exec("ab<cd"); return [RegExp.lastMatch, RegExp.leftContext, RegExp.rightContext, RegExp["$_"]].join("|"); },
  staticsGlobal: () => { const rx = /[0-9]+/g; rx.exec("a12b345c"); rx.exec("a12b345c"); return [RegExp.lastMatch, RegExp.leftContext, RegExp.rightContext].join("|") + ":" + rx.lastIndex; },
  recompileToComplex: () => { const rx = /a/g; rx.lastIndex = { valueOf() { rx.compile("(b)+", "g"); return 0; } }; return show(rx.exec("abbb")) + ":" + rx.lastIndex; },
  recompileToSimple: () => { const rx = /(b)+/g; rx.lastIndex = { valueOf() { rx.compile("[a]", "g"); return 1; } }; return show(rx.exec("abab")) + ":" + rx.lastIndex; },
  recompileToSticky: () => { const rx = /a/g; rx.lastIndex = { valueOf() { rx.compile("a", "y"); return 1; } }; return show(rx.exec("ba")) + ":" + rx.lastIndex; },
  testGlobal: () => { const rx = /[<>]/g, s = "<a>"; return [rx.test(s), rx.lastIndex, rx.test(s), rx.lastIndex, rx.test(s), rx.lastIndex].join(); },
  matchNonGlobal: () => show("a1b22".match(/\d+/)),
  utf16Subject: () => { const rx = /[é<]/g, s = "aé<ü"; const out = []; let m; while ((m = rx.exec(s))) out.push(show(m) + ":" + rx.lastIndex); return out.join(" "); },
};
Object.entries(cases).map(([name, f]) => name + "\t" + f()).join("\n")`

const simpleExecWant = `escape	["<"]@1,0
global	["<"]@1:2 ["\""]@2:3 ["'"]@4:5 0
past	null,0
sticky	["a"]@1:2 null:0
stickyGlobal	["abc"]@0:3 null:0
ignoreCase	["ABC"]@1
indices	["<>"]@1[[1,3]]
surrogateUnits	1:d83d:2 2:de00:3 4:d800:5
surrogateU	4:d800:5
seroval	false,true,true,true,true,true,false
insidePairU	["y"]@3:4
statics	<|ab|cd|ab<cd
staticsGlobal	345|a12b|c:7
recompileToComplex	["bbb","b"]@1:4
recompileToSimple	["a"]@2:3
recompileToSticky	["a"]@1:2
testGlobal	true,1,true,3,false,0
matchNonGlobal	["1"]@1
utf16Subject	["é"]@1:2 ["<"]@2:3`

// TestSimpleClassExec checks what exec records on the single-class path
// against Node.js 24: the result array (index, input, groups, indices with
// the d flag), lastIndex under g and y and past the end, the legacy
// statics, case folding under i (the translator's), surrogates matched per
// unit without u and as code points with it, and a lastIndex whose valueOf
// recompiles the RegExp into another kind of program.
func TestSimpleClassExec(t *testing.T) {
	assert.Equal(t, simpleExecWant, runScripts(t, simpleExecCases))
	r := NewRealm()
	for _, c := range []struct {
		src, flags string
		simple     bool
	}{{`["'&<>]`, "", true}, {`[\ud800-\udfff]`, "g", true}, {`[\ud800-\udfff]`, "gu", false}, {`[a-c]+`, "i", false},
		{`[\x00-\x07\x0b\x0e-\x1f<\u2028\u2029\ud800-\udfff]`, "", true}} {
		rx, err := r.NewRegExp(FromGoString(c.src), FromGoString(c.flags))
		require.NoError(t, err)
		assert.Equal(t, c.simple, rx.RegExpData().c.simple != nil, "/%s/%s", c.src, c.flags)
	}
}

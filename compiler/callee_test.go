package compiler

import (
	"math"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// calleeNames describes the callee of every call and new instruction of fn
// in code order.
func calleeNames(fn *bytecode.Function) []string {
	var out []string
	for pc := 0; pc < len(fn.Code); {
		op := bytecode.DecodeOp(fn.Code[pc])
		switch op {
		case bytecode.Call, bytecode.CallSpread, bytecode.New, bytecode.NewSpread:
			out = append(out, describeCallee(fn, uint32(pc)))
		}
		pc += 1 + op.ExtraWords()
	}
	return out
}

func TestDescribeCallee(t *testing.T) {
	fn := compileModule(t, "const o = {}, a = [], k = 0;\n"+
		"o.m(); o.a.b.c(); a[0](); a[\"x\"](); a[k](); a[`t`]();\n"+
		"o?.m(); o.p?.q.r(); o.m?.(); f()(); f(1)(2)(3); tag`a``b`; o.t`x`;\n"+
		"new o.C(); new new o.D()(); new (() => 1)(); f(...a); new o.C(...a);\n"+
		"(0, o.m)(); (o?.m)(); (-1).x(); (!0).x(); (1 + 2 + o.n).x(); (o.a + o.b + o.c)();\n"+
		"(o.a + (o.b + o.c))(); (o.a < o.b)(); (o.a && o.b || o.c)(); (o.a ?? o.b ?? o.c)();\n"+
		"[1, , ...a].x(); ({a: 1, b() {}}).x(); (function () {}).x(); (o.x = 1)(); ([o.p, ...o.q] = a)();\n"+
		"(o ? 1 : 2)(); (typeof o)(); (void 0)(); (o.n++)(); (--o.n)(); `s`.x(); `${o}t${k}`.x();\n"+
		"/a/gi.x(); this.x(); (null)(); (1e21)(); (0.000001)(); (1e-7)(); (0x10)(); 10n.x();\n"+
		"function g(p = o.dflt()) { return o.inner(); }\n"+
		"const h = () => o.arrow();\n")
	assert.Equal(t, []string{
		"o.m", "o.a.b.c", "a[0]", "a.x", "a[k]", "a.t",
		"o?.m", "o.p?.q.r", "o.m", "f", "f(...)", "f", "f(...)", "f(...)(...)", "tag", "tag(...)", "o.t",
		"o.C", "o.D", "(intermediate value)", "(intermediate value)", "f", "o.C",
		"(0 , o.m)", "(intermediate value)", "-1.x", "true.x", "(3 + o.n).x", "(o.a + o.b + o.c)",
		"(o.a + (o.b + o.c))", "(o.a < o.b)", "((o.a && o.b) || o.c)", "(o.a ?? o.b ?? o.c)",
		"[1,(intermediate value),(...a)].x", "{(intermediate value)(intermediate value)}.x", "(intermediate value).x", "o.x", "[o.p,(...o.q)]",
		"(intermediate value)(intermediate value)(intermediate value)", "(typeof o)", "(void 0)", "(o.n++)", "(--o.n)", `"s".x`, "ok.x",
		"/a/gi.x", "this.x", "null", "1e+21", "0.000001", "1e-7", "16", "(intermediate value).x",
	}, calleeNames(fn))
	assert.Equal(t, []string{"o.dflt", "o.inner"}, calleeNames(child(t, fn, "g")))
	assert.Equal(t, []string{"o.arrow"}, calleeNames(child(t, fn, "h")))

	s, err := syntax.ParseScript("s.js", "var await = {};\nawait.m();", syntax.Options{})
	require.NoError(t, err)
	script, err := CompileScript(s)
	require.NoError(t, err)
	assert.Equal(t, []string{"await.m"}, calleeNames(script), "a script that is not a module")
}

// TestDescribeCalleeClass covers the code whose source is its class's: the
// constructor, the field and static initializers, and a static block.
func TestDescribeCalleeClass(t *testing.T) {
	fn := compileModule(t, "const o = {};\n"+
		"class C extends o.B() {\n"+
		"  constructor() { super(); o.ctor(); }\n"+
		"  a = o.field(); [o.key()] = o.computed(); f = () => o.arrow(); g;\n"+
		"  static s = o.static();\n"+
		"  static { o.block(); }\n"+
		"  m() { o.method(); }\n"+
		"}\n")
	assert.Equal(t, []string{"o.B", "o.key", ""}, calleeNames(fn), "the heritage and a computed key; the static initializer runs unnamed")
	assert.Equal(t, []string{"", "o.ctor"}, calleeNames(child(t, fn, "C")), "super() runs the field initializer unnamed")
	fields := child(t, fn, "<instance_members_initializer>")
	assert.Equal(t, []string{"o.field", "o.computed"}, calleeNames(fields))
	assert.Equal(t, []string{"o.arrow"}, calleeNames(child(t, fields, "f")))
	static := child(t, fn, "<static_initializer>")
	assert.Equal(t, []string{"o.static", ""}, calleeNames(static), "the static block runs unnamed")
	assert.Equal(t, []string{"o.block"}, calleeNames(child(t, static, "")))
}

// TestDescribeCalleeUnknown covers the sites that cannot be told apart and
// the templates without a usable source: they are not described.
func TestDescribeCalleeUnknown(t *testing.T) {
	fn := compileModule(t, "f();")
	require.Equal(t, []string{"f"}, calleeNames(fn))
	call := bytecode.EncodeABC(bytecode.Call, 0, 0, 0)
	twice := *fn
	twice.Code = []uint32{call, call}
	twice.LineTable = []bytecode.LineEntry{{PC: 0, Line: 1, Col: 1}}
	assert.Equal(t, []string{"", ""}, calleeNames(&twice), "two calls, one site")

	once := twice
	once.Code = []uint32{call}
	assert.Equal(t, []string{"f"}, calleeNames(&once))

	noSource := once
	noSource.Source = nil
	assert.Equal(t, []string{""}, calleeNames(&noSource))

	bad := once
	bad.Source = &bytecode.SourceInfo{Name: "t.js", Src: "f((", Start: 0, End: 3}
	assert.Equal(t, []string{""}, calleeNames(&bad), "unparsable source")

	lost := once
	lost.Kind = bytecode.KindNormal
	assert.Equal(t, []string{""}, calleeNames(&lost), "no function spans the source range")
	assert.Equal(t, "", describeCallee(&once, uint32(len(once.Code))))
}

func TestNumberString(t *testing.T) {
	for _, tt := range []struct {
		v    float64
		want string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {1, "1"}, {-1.5, "-1.5"}, {123.456, "123.456"},
		{1e6, "1000000"}, {1e20, "100000000000000000000"}, {1e21, "1e+21"}, {1.5e300, "1.5e+300"},
		{0.001, "0.001"}, {1e-6, "0.000001"}, {1e-7, "1e-7"}, {1.25e-10, "1.25e-10"},
		{math.NaN(), "NaN"}, {math.Inf(1), "Infinity"}, {math.Inf(-1), "-Infinity"},
	} {
		assert.Equal(t, tt.want, numberString(tt.v), "%v", tt.v)
	}
}

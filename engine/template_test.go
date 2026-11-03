package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaggedTemplate checks tagged-template calls and GetTemplateObject: the
// frozen cooked array with its frozen, non-enumerable raw array, undefined
// cooked values for invalid escapes, one object per site per realm, the
// member-call this value, new with a tag, and the evaluation order.
func TestTaggedTemplate(t *testing.T) {
	f := evalModule(t, `
const J = JSON.stringify;
function desc(o, k) { const d = Object.getOwnPropertyDescriptor(o, k); return [d.writable, d.enumerable, d.configurable]; }
function tag(s, ...v) { return J([s, s.raw, v]); }
export function basic() { return tag`+"`a${1}b${'x'}c`"+`; }
export function empty() { return tag`+"``"+`; }
export function shape() {
	const s = (x => x)`+"`a${0}b`"+`;
	return J([Array.isArray(s), Object.isFrozen(s), Object.isFrozen(s.raw), Object.keys(s), desc(s, "raw"), desc(s, 0), desc(s.raw, 1), Object.getPrototypeOf(s.raw) === Array.prototype, s.length]);
}
export function invalid() { return tag`+"`\\unicode ${1} \\u{110000}\\xg\\01\\9 ok\\n`"+`; }
export function lines() { return tag`+"`a\r\nb\\\r\nc`"+`; }
export function lone() { const s = (x => x)`+"`\\uD800`"+`; return [s[0].length, s[0].charCodeAt(0), s.raw[0]].join(); }
function id(s) { return s; }
function site() { return id`+"`x`"+`; }
export function identity() { return J([site() === site(), id`+"`x`"+` === id`+"`x`"+`, site() !== id`+"`x`"+`]); }
export function loopIdentity() { let first, same = true; for (let i = 0; i < 3; i++) { const s = id`+"`y${i}`"+`; if (i === 0) first = s; else same = same && s === first; } return same; }
export function member() { const o = { m(s) { return this === o && s[0]; } }; return o.m`+"`hi`"+` + o["m"]`+"`!`"+`; }
export function construct() {
	function C(x) { this.x = x; }
	function mk(s) { mk.s = s[0]; return C; }
	const a = new mk`+"`one`"+`, b = new mk`+"`two`"+`(5);
	return J([a instanceof C, a.x === undefined, b.x, mk.s]);
}
export function chained() { const t = s => u => s[0] + u[0]; return t`+"`a``b`"+`; }
export function order() {
	const log = [];
	const o = { get t() { log.push("get"); return (s, ...v) => { log.push("call"); return v.length; } } };
	o.t`+"`${log.push('sub1')}${log.push('sub2')}`"+`;
	try { undefined`+"`${log.push('sub3')}`"+`; } catch (e) { log.push(e.name); }
	return log.join();
}
export function frozenWrite() { const s = id`+"`z`"+`; try { s[0] = 1; return "no"; } catch (e) { return e.name + s[0]; } }
export function fresh() { return id`+"`w`"+`; }
`)
	assert.Equal(t, `[["a","b","c"],["a","b","c"],[1,"x"]]`, f.call("basic"))
	assert.Equal(t, `[[""],[""],[]]`, f.call("empty"))
	assert.Equal(t, `[true,true,true,["0","1"],[false,false,false],[false,true,false],[false,true,false],true,2]`, f.call("shape"))
	assert.Equal(t, `[[null,null],["\\unicode "," \\u{110000}\\xg\\01\\9 ok\\n"],[1]]`, f.call("invalid"))
	assert.Equal(t, `[["a\nbc"],["a\nb\\\nc"],[]]`, f.call("lines"))
	assert.Equal(t, "1,55296,\\uD800", f.call("lone"))
	assert.Equal(t, `[true,false,true]`, f.call("identity"))
	assert.Equal(t, true, f.call("loopIdentity"))
	assert.Equal(t, "hi!", f.call("member"))
	assert.Equal(t, `[true,true,5,"two"]`, f.call("construct"))
	assert.Equal(t, "ab", f.call("chained"))
	assert.Equal(t, "get,sub1,sub2,call,sub3,TypeError", f.call("order"))
	assert.Equal(t, "TypeErrorz", f.call("frozenWrite"))

	// The same compiled site yields a distinct object in another realm.
	m, err := syntax.ParseModule("t.js", `function id(s) { return s; } export function fresh() { return id`+"`w`"+`; }`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	var objs []*Object
	for _, opts := range []RealmOptions{{}, {}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		fn, _ := env.GetBindingValue("fresh")
		v1, err := r.Call(fn, Undefined(), nil)
		require.NoError(t, err)
		v2, err := r.Call(fn, Undefined(), nil)
		require.NoError(t, err)
		require.Same(t, v1.AsObject(), v2.AsObject())
		objs = append(objs, v1.AsObject())
	}
	assert.NotSame(t, objs[0], objs[1])
	assert.NotSame(t, objs[1], objs[2])
}

// TestTaggedTemplateCost checks that the template object is built once per
// site and realm (later evaluations allocate nothing) and that
// code without tagged templates leaves the realm's cache untouched.
func TestTaggedTemplateCost(t *testing.T) {
	skipInRegcheck(t)
	allocs, _, _ := callAllocs(t, "function id(s) { return s; } export function f() { return id`a${1}b`; }")
	assert.Equal(t, 0.0, allocs, "cached template object")

	f := evalModule(t, "export function f(a) { return `a${a}b`.length; }")
	assert.Equal(t, int64(3), f.call("f", "x"))
	assert.True(t, f.r.lazy == nil || f.r.lazy.templates == nil, "untagged templates do not touch the cache")
}
